// Package otlp registers an "otlp" exporter for mkot-built providers, pushing
// metrics, traces, and logs to an OTLP/gRPC collector endpoint. It depends only
// on mkot and OpenTelemetry so it can be extracted verbatim into
// github.com/lesomnus/mkot/exporters/otlp.
//
//	otel:
//	  exporters:
//	    otlp: { endpoint: "127.0.0.1:4317", tls: { insecure: true } }
//	  providers:
//	    meter: { exporters: [otlp] }
package otlp

import (
	"context"
	"fmt"
	"time"

	"github.com/lesomnus/mkot"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"
)

var _ mkot.ExporterConfig = (*ExporterConfig)(nil)

type ExporterConfig struct {
	mkot.UnimplementedExporterConfig `yaml:"-"`

	// Endpoint is the collector's OTLP/gRPC address as host:port. Required.
	Endpoint string `yaml:"endpoint"`

	// TLS configures transport security; set `tls: { insecure: true }` for a
	// plaintext connection. Defaults to TLS against the system root CAs.
	TLS mkot.ClientTlsConfig `yaml:"tls,omitempty"`

	// Headers are added to every export request (e.g. authorization).
	Headers map[string]string `yaml:"headers,omitempty"`

	// Timeout bounds a single export request. Zero uses the exporter default (10s).
	Timeout time.Duration `yaml:"timeout,omitempty"`

	// Queue batches spans and logs before export, like the collector's
	// sending_queue. Metrics are unaffected (they are pushed every Interval).
	Queue mkot.QueueConfig `yaml:"sending_queue,omitempty"`

	// Interval is the metric push period. Zero uses the SDK default (60s).
	Interval time.Duration `yaml:"interval,omitempty"`

	// Temporality selects the metric aggregation temporality: "cumulative"
	// (default) or "delta".
	Temporality string `yaml:"temporality,omitempty"`
}

func (e ExporterConfig) validate() error {
	if e.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	switch e.Temporality {
	case "", "cumulative", "delta":
		return nil
	default:
		return fmt.Errorf("unknown temporality %q (want cumulative or delta)", e.Temporality)
	}
}

// creds builds the gRPC transport credentials, or nil for a plaintext connection.
func (e ExporterConfig) creds() (credentials.TransportCredentials, error) {
	if e.TLS.Insecure {
		return nil, nil
	}
	tc, err := e.TLS.Build()
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	// mkot's ClientTlsConfig.Build fills the server-side ClientCAs field, but a
	// client handshake verifies the collector against RootCAs — without this
	// move a configured ca_file/ca_pem is silently ignored. Drop once fixed
	// upstream (before extracting this package into mkot).
	if tc.RootCAs == nil && tc.ClientCAs != nil {
		tc.RootCAs = tc.ClientCAs
		tc.ClientCAs = nil
	}
	return credentials.NewTLS(tc), nil
}

// MetricReader wires a periodic OTLP push. The reader is returned as the
// lifecycle component so its Shutdown flushes the final collection before
// closing the exporter.
func (e ExporterConfig) MetricReader(ctx context.Context) (metric.Reader, []metric.Option, error) {
	if err := e.validate(); err != nil {
		return nil, nil, err
	}
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(e.Endpoint)}
	creds, err := e.creds()
	if err != nil {
		return nil, nil, err
	}
	if creds == nil {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	} else {
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(creds))
	}
	if len(e.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(e.Headers))
	}
	if e.Timeout > 0 {
		opts = append(opts, otlpmetricgrpc.WithTimeout(e.Timeout))
	}
	if e.Temporality == "delta" {
		opts = append(opts, otlpmetricgrpc.WithTemporalitySelector(deltaTemporality))
	}
	v, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}
	ropts := []metric.PeriodicReaderOption{}
	if e.Interval > 0 {
		ropts = append(ropts, metric.WithInterval(e.Interval))
	}
	r := metric.NewPeriodicReader(v, ropts...)
	return r, []metric.Option{metric.WithReader(r)}, nil
}

func (e ExporterConfig) SpanExporter(ctx context.Context) (trace.SpanExporter, []trace.TracerProviderOption, error) {
	if err := e.validate(); err != nil {
		return nil, nil, err
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(e.Endpoint)}
	creds, err := e.creds()
	if err != nil {
		return nil, nil, err
	}
	if creds == nil {
		opts = append(opts, otlptracegrpc.WithInsecure())
	} else {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(creds))
	}
	if len(e.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(e.Headers))
	}
	if e.Timeout > 0 {
		opts = append(opts, otlptracegrpc.WithTimeout(e.Timeout))
	}
	// Unstarted: the resolver's Start is the single starter (New(ctx) starts the
	// exporter itself, and a second Start fails with "already started").
	v := otlptracegrpc.NewUnstarted(opts...)
	p := e.spanProcessor(v)
	return spanComponent{v, p}, []trace.TracerProviderOption{trace.WithSpanProcessor(p)}, nil
}

// spanComponent is the lifecycle handle the resolver manages: Shutdown drains
// the processor (which flushes batched spans, then closes the exporter) instead
// of closing the exporter under the processor's feet.
type spanComponent struct {
	trace.SpanExporter
	p trace.SpanProcessor
}

func (c spanComponent) Start(ctx context.Context) error {
	s, ok := c.SpanExporter.(interface{ Start(context.Context) error })
	if !ok {
		return nil
	}
	return s.Start(ctx)
}

func (c spanComponent) Shutdown(ctx context.Context) error { return c.p.Shutdown(ctx) }

func (e ExporterConfig) LogExporter(ctx context.Context) (log.Exporter, []log.LoggerProviderOption, error) {
	if err := e.validate(); err != nil {
		return nil, nil, err
	}
	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(e.Endpoint)}
	creds, err := e.creds()
	if err != nil {
		return nil, nil, err
	}
	if creds == nil {
		opts = append(opts, otlploggrpc.WithInsecure())
	} else {
		opts = append(opts, otlploggrpc.WithTLSCredentials(creds))
	}
	if len(e.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(e.Headers))
	}
	if e.Timeout > 0 {
		opts = append(opts, otlploggrpc.WithTimeout(e.Timeout))
	}
	v, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}
	p := e.logProcessor(v)
	return logComponent{v, p}, []log.LoggerProviderOption{log.WithProcessor(p)}, nil
}

// logComponent mirrors spanComponent: Shutdown drains the processor so batched
// records are flushed before the exporter closes.
type logComponent struct {
	log.Exporter
	p log.Processor
}

func (c logComponent) Shutdown(ctx context.Context) error { return c.p.Shutdown(ctx) }

// spanProcessor is QueueConfig.BuildSpanProcessor with the SDK defaults kept for
// unset values — a batcher built with a zero max-queue drops every span.
func (e ExporterConfig) spanProcessor(v trace.SpanExporter) trace.SpanProcessor {
	c := e.Queue
	if !c.IsEnabled() {
		return trace.NewSimpleSpanProcessor(v)
	}
	opts := []trace.BatchSpanProcessorOption{}
	if c.QueueSize > 0 {
		opts = append(opts, trace.WithMaxQueueSize(int(c.QueueSize)))
	}
	if c.Batch.FlushTimeout > 0 {
		opts = append(opts, trace.WithBatchTimeout(c.Batch.FlushTimeout))
	}
	if c.Batch.MaxSize > 0 {
		opts = append(opts, trace.WithMaxExportBatchSize(int(c.Batch.MaxSize)))
	}
	return trace.NewBatchSpanProcessor(v, opts...)
}

// logProcessor mirrors spanProcessor for the log pipeline.
func (e ExporterConfig) logProcessor(v log.Exporter) log.Processor {
	c := e.Queue
	if !c.IsEnabled() {
		return log.NewSimpleProcessor(v)
	}
	opts := []log.BatchProcessorOption{}
	if c.QueueSize > 0 {
		opts = append(opts, log.WithMaxQueueSize(int(c.QueueSize)))
	}
	if c.Batch.FlushTimeout > 0 {
		opts = append(opts, log.WithExportInterval(c.Batch.FlushTimeout))
	}
	if c.Batch.MaxSize > 0 {
		opts = append(opts, log.WithExportMaxBatchSize(int(c.Batch.MaxSize)))
	}
	return log.NewBatchProcessor(v, opts...)
}

// deltaTemporality is the standard delta preference: sums and histograms report
// per-interval deltas; up-down counters stay cumulative (a delta of a level is
// meaningless).
func deltaTemporality(k metric.InstrumentKind) metricdata.Temporality {
	switch k {
	case metric.InstrumentKindUpDownCounter, metric.InstrumentKindObservableUpDownCounter:
		return metricdata.CumulativeTemporality
	default:
		return metricdata.DeltaTemporality
	}
}

func init() {
	mkot.DefaultExporterRegistry.Set("otlp", func() mkot.ExporterConfig {
		return &ExporterConfig{}
	})
}
