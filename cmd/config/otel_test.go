package config

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/mkot"
	"github.com/lesomnus/otx"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
)

// metricSink records pushed metric names and resource attributes.
type metricSink struct {
	collectormetricspb.UnimplementedMetricsServiceServer

	mu    sync.Mutex
	names map[string]bool
	attrs map[string]string
}

func (f *metricSink) Export(ctx context.Context, req *collectormetricspb.ExportMetricsServiceRequest) (*collectormetricspb.ExportMetricsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names == nil {
		f.names = map[string]bool{}
		f.attrs = map[string]string{}
	}
	for _, rm := range req.ResourceMetrics {
		if rm.Resource != nil {
			for _, kv := range rm.Resource.Attributes {
				f.attrs[kv.Key] = kv.Value.GetStringValue()
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				f.names[m.Name] = true
			}
		}
	}
	return &collectormetricspb.ExportMetricsServiceResponse{}, nil
}

// A user config that wires its own meter provider must still push with the
// gantry service resource attached, and shutdown must flush pending metrics.
func TestOtelBuildOtlpMeter(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &metricSink{}
	srv := grpc.NewServer()
	collectormetricspb.RegisterMetricsServiceServer(srv, sink)
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
	var mc mkot.Config
	if err := yaml.Unmarshal([]byte(src), &mc); err != nil {
		t.Fatal(err)
	}
	oc := &OtelConfig{Config: mc}
	ctx, o, err := oc.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctr, err := otx.Meter(ctx).Int64Counter("gantry.test.count")
	if err != nil {
		t.Fatal(err)
	}
	ctr.Add(ctx, 1)
	if err := o.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.names["gantry.test.count"] {
		t.Error("collector did not receive the counter; shutdown should flush the periodic reader")
	}
	if sink.attrs["service.name"] != "gantry" {
		t.Errorf("service.name = %q, want %q (resource processor must be prepended to user-defined providers)", sink.attrs["service.name"], "gantry")
	}
}
