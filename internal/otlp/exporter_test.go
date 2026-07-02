package otlp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/mkot"
	"github.com/lesomnus/mkot/opaque"
	olog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestConfigDecode(t *testing.T) {
	src := `
exporters:
  otlp:
    endpoint: "collector.local:4317"
    tls: { insecure: true }
    headers: { authorization: "Bearer x" }
    timeout: 5s
    interval: 30s
    temporality: delta
providers:
  meter:
    exporters: [otlp]
`
	var c mkot.Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	e, ok := c.Exporters[mkot.Id("otlp")].(*ExporterConfig)
	if !ok {
		t.Fatalf("exporter = %T, want *ExporterConfig", c.Exporters[mkot.Id("otlp")])
	}
	if e.Endpoint != "collector.local:4317" {
		t.Errorf("endpoint = %q", e.Endpoint)
	}
	if !e.TLS.Insecure {
		t.Error("tls.insecure not decoded")
	}
	if e.Headers["authorization"] != "Bearer x" {
		t.Errorf("headers = %v", e.Headers)
	}
	if e.Timeout != 5*time.Second || e.Interval != 30*time.Second {
		t.Errorf("timeout = %v, interval = %v", e.Timeout, e.Interval)
	}
	if e.Temporality != "delta" {
		t.Errorf("temporality = %q", e.Temporality)
	}
}

func TestValidate(t *testing.T) {
	ctx := context.Background()
	t.Run("endpoint required", func(t *testing.T) {
		if _, _, err := (ExporterConfig{}).MetricReader(ctx); err == nil || !strings.Contains(err.Error(), "endpoint") {
			t.Errorf("err = %v, want endpoint required", err)
		}
	})
	t.Run("unknown temporality", func(t *testing.T) {
		e := ExporterConfig{Endpoint: "x:1", Temporality: "bogus"}
		if _, _, err := e.MetricReader(ctx); err == nil || !strings.Contains(err.Error(), "temporality") {
			t.Errorf("err = %v, want temporality error", err)
		}
	})
}

func TestDeltaTemporality(t *testing.T) {
	if got := deltaTemporality(metric.InstrumentKindCounter); got != metricdata.DeltaTemporality {
		t.Errorf("counter = %v, want delta", got)
	}
	if got := deltaTemporality(metric.InstrumentKindHistogram); got != metricdata.DeltaTemporality {
		t.Errorf("histogram = %v, want delta", got)
	}
	if got := deltaTemporality(metric.InstrumentKindUpDownCounter); got != metricdata.CumulativeTemporality {
		t.Errorf("updown = %v, want cumulative", got)
	}
}

// fakeCollector records the metric names pushed to it over OTLP/gRPC.
type fakeCollector struct {
	collectormetricspb.UnimplementedMetricsServiceServer

	mu    sync.Mutex
	names map[string]bool
}

func (f *fakeCollector) Export(ctx context.Context, req *collectormetricspb.ExportMetricsServiceRequest) (*collectormetricspb.ExportMetricsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if f.names == nil {
					f.names = map[string]bool{}
				}
				f.names[m.Name] = true
			}
		}
	}
	return &collectormetricspb.ExportMetricsServiceResponse{}, nil
}

func (f *fakeCollector) seen(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names[name]
}

// traceSink records span names pushed over OTLP/gRPC.
type traceSink struct {
	collectortracepb.UnimplementedTraceServiceServer

	mu    sync.Mutex
	names map[string]bool
}

func (f *traceSink) Export(ctx context.Context, req *collectortracepb.ExportTraceServiceRequest) (*collectortracepb.ExportTraceServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				if f.names == nil {
					f.names = map[string]bool{}
				}
				f.names[s.Name] = true
			}
		}
	}
	return &collectortracepb.ExportTraceServiceResponse{}, nil
}

// The resolver's Start must be the exporter's single starter (a self-started
// exporter fails Start with "already started"), and Shutdown must drain the
// default batch processor so the last spans are not dropped.
func TestSpanPushEndToEnd(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &traceSink{}
	srv := grpc.NewServer()
	collectortracepb.RegisterTraceServiceServer(srv, sink)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	src := `
exporters:
  otlp:
    endpoint: "` + lis.Addr().String() + `"
    tls: { insecure: true }
providers:
  tracer:
    exporters: [otlp]
`
	var c mkot.Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := mkot.Make(ctx, &c)
	tp, err := r.Tracer(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, span := tp.Tracer("test").Start(ctx, "gantry.test.span")
	span.End()
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.names["gantry.test.span"] {
		t.Error("collector did not receive the span; shutdown must flush the batch processor")
	}
}

// logSink records log bodies pushed over OTLP/gRPC.
type logSink struct {
	collectorlogspb.UnimplementedLogsServiceServer

	mu     sync.Mutex
	bodies map[string]bool
}

func (f *logSink) Export(ctx context.Context, req *collectorlogspb.ExportLogsServiceRequest) (*collectorlogspb.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				if f.bodies == nil {
					f.bodies = map[string]bool{}
				}
				f.bodies[lr.Body.GetStringValue()] = true
			}
		}
	}
	return &collectorlogspb.ExportLogsServiceResponse{}, nil
}

func TestLogPushEndToEnd(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &logSink{}
	srv := grpc.NewServer()
	collectorlogspb.RegisterLogsServiceServer(srv, sink)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	src := `
exporters:
  otlp:
    endpoint: "` + lis.Addr().String() + `"
    tls: { insecure: true }
providers:
  logger:
    exporters: [otlp]
`
	var c mkot.Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := mkot.Make(ctx, &c)
	lp, err := r.Logger(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	var rec olog.Record
	rec.SetBody(olog.StringValue("gantry.test.log"))
	lp.Logger("test").Emit(ctx, rec)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.bodies["gantry.test.log"] {
		t.Error("collector did not receive the record; shutdown must flush the batch processor")
	}
}

// selfSignedCert issues a CA-capable self-signed cert for 127.0.0.1, returned
// as a server keypair plus its PEM (usable as the client's trust anchor).
func selfSignedCert(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gantry-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert_pem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key_der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	key_pem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key_der})
	pair, err := tls.X509KeyPair(cert_pem, key_pem)
	if err != nil {
		t.Fatal(err)
	}
	return pair, cert_pem
}

// A configured ca_pem must be used as the client's trust anchor (mkot's Build
// puts the pool in the server-side field; creds() has to move it to RootCAs).
func TestTLSCustomCA(t *testing.T) {
	pair, ca_pem := selfSignedCert(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeCollector{}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}})))
	collectormetricspb.RegisterMetricsServiceServer(srv, sink)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	e := ExporterConfig{
		Endpoint: lis.Addr().String(),
		TLS:      mkot.ClientTlsConfig{TLSConfig: mkot.TLSConfig{CAPem: opaque.String(ca_pem)}},
		Interval: time.Hour,
	}
	ctx := context.Background()
	_, opts, err := e.MetricReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mp := metric.NewMeterProvider(opts...)
	t.Cleanup(func() { mp.Shutdown(context.Background()) })
	ctr, err := mp.Meter("test").Int64Counter("gantry.test.tls")
	if err != nil {
		t.Fatal(err)
	}
	ctr.Add(ctx, 1)
	if err := mp.ForceFlush(ctx); err != nil {
		t.Fatalf("push over TLS with a custom CA failed: %v", err)
	}
	if !sink.seen("gantry.test.tls") {
		t.Error("collector did not receive the counter")
	}
}

func TestMetricPushEndToEnd(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	col := &fakeCollector{}
	srv := grpc.NewServer()
	collectormetricspb.RegisterMetricsServiceServer(srv, col)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	src := `
exporters:
  otlp:
    endpoint: "` + lis.Addr().String() + `"
    tls: { insecure: true }
    interval: 1h
providers:
  meter:
    exporters: [otlp]
`
	var c mkot.Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := mkot.Make(ctx, &c)
	t.Cleanup(func() { r.Shutdown(context.Background()) })

	mp, err := r.Meter(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := mp.Meter("test").Int64Counter("gantry.test.count")
	if err != nil {
		t.Fatal(err)
	}
	ctr.Add(ctx, 42)

	sdk_mp, ok := mp.(*metric.MeterProvider)
	if !ok {
		t.Fatalf("meter provider = %T, want *metric.MeterProvider (an exporter must be wired)", mp)
	}
	if err := sdk_mp.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	if !col.seen("gantry.test.count") {
		t.Error("collector did not receive the pushed counter")
	}
}
