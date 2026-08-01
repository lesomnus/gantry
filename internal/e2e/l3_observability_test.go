//go:build e2e

// The routing features' observability, end to end: the shipped binary exporting
// over real OTLP into a receiver this test runs, and the durable audit log.
//
// gantry.job.route is the only record of a decision gantry makes for itself —
// a job whose route was declined looks, from its own snapshot, exactly like a
// job that was never eligible for one. Nothing but a metrics pipeline can tell
// them apart, so nothing but a metrics pipeline can test it.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"

	"github.com/lesomnus/gantry/pb"
)

// otlpSink is an OTLP/gRPC metrics receiver that keeps every metric it is sent.
type otlpSink struct {
	colmetricpb.UnimplementedMetricsServiceServer
	mu  sync.Mutex
	got []*metricpb.Metric
}

func (s *otlpSink) Export(_ context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			s.got = append(s.got, sm.GetMetrics()...)
		}
	}
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

// counted reports the summed value of a counter's data points whose attributes
// include every want pair, and the attribute sets it did see (for the failure
// message).
func (s *otlpSink) counted(name string, want map[string]string) (total float64, seen []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.got {
		if m.GetName() != name {
			continue
		}
		for _, dp := range m.GetSum().GetDataPoints() {
			attrs := map[string]string{}
			for _, kv := range dp.GetAttributes() {
				attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
			}
			seen = append(seen, fmt.Sprint(attrs))
			ok := true
			for k, v := range want {
				if attrs[k] != v {
					ok = false
					break
				}
			}
			if ok {
				total += float64(dp.GetAsInt()) + dp.GetAsDouble()
			}
		}
	}
	return total, seen
}

// startOTLPSink serves the OTLP metrics service on loopback and returns its
// address.
func startOTLPSink(t *testing.T) (*otlpSink, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("otlp listen: %v", err)
	}
	sink := &otlpSink{}
	srv := grpc.NewServer()
	colmetricpb.RegisterMetricsServiceServer(srv, sink)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	return sink, l.Addr().String()
}

// awaitCounter polls until a counter with the wanted attributes has been
// exported, or the deadline passes.
func awaitCounter(t *testing.T, sink *otlpSink, name string, want map[string]string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		total, seen := sink.counted(name, want)
		if total > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s%v was never exported; the data points that were: %v", name, want, seen)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestL3RoutingObservability(t *testing.T) {
	cli := dockerClientOrSkip(t)
	daemonHost, needFwd := remoteDaemon()
	remote := startRegistryContainer(t, cli, daemonHost, needFwd)
	cache := startRegistryContainer(t, cli, daemonHost, needFwd)
	cold := startRegistryContainer(t, cli, daemonHost, needFwd)
	seedImage(t, remote, "lib/app", "1")
	seedImage(t, remote, "lib/other", "1")

	sink, otlpAddr := startOTLPSink(t)

	bin := buildGantry(t)
	dir := t.TempDir()
	addr := "127.0.0.1:" + freePort(t)
	cfgPath := filepath.Join(dir, "gantry-e2e.yaml")
	// A one-second push interval so the assertions do not wait out the 60s default.
	cfg := fmt.Sprintf(`otel:
  exporters:
    otlp:
      endpoint: %q
      tls: { insecure: true }
      interval: 1s
  providers:
    meter: { exporters: [otlp] }
serve:
  addr: %q
  events:
    path: %q
worker:
  fallback_to_origin: true
stores:
  remote: { kind: "oci", host: %q, insecure: true, cache: "cache" }
  cache: { kind: "oci", host: %q, insecure: true, mode: "copy" }
  cold: { kind: "oci", host: %q, insecure: true }
  edge: { kind: "docker", address: %q }
`, otlpAddr, addr, filepath.Join(dir, "events.db"), remote, cache, cold, dockerAddr())
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	_, client, stop := runGantry(t, bin, cfgPath, addr)
	defer stop()

	submit := func(t *testing.T, ref, source string) {
		t.Helper()
		job, err := client.Job().Add(context.Background(), pb.JobAddRequest_builder{
			Ref:    ref,
			Source: pb.StoreByName(source),
			Target: pb.StoreByName("edge"),
		}.Build())
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		waitTerminal(t, client, job.GetId())
	}

	// A cold cache gantry chose to fill: decision "filled".
	submit(t, remote+"/lib/app:1", "remote")
	awaitCounter(t, sink, "gantry.job.route", map[string]string{
		"decision": "filled", "source": "remote", "cache": "cache",
	})

	// The same origin again, now warm: decision "warm". The two together are the
	// thing the counter exists for — a route taken twice for different reasons.
	submit(t, remote+"/lib/app:1", "remote")
	awaitCounter(t, sink, "gantry.job.route", map[string]string{
		"decision": "warm", "source": "remote", "cache": "cache",
	})

	// A source that cannot serve, with the worker default supplying the fallback:
	// the fallback counter records that the origin had to be read.
	submit(t, remote+"/lib/other:1", "cold")
	awaitCounter(t, sink, "gantry.job.fallback", nil)

	// And the same fact is on the durable record, which outlives the process.
	res, err := client.Event().List(context.Background(), &pb.EventListRequest{})
	if err != nil {
		t.Fatalf("event list: %v", err)
	}
	found := false
	var types []string
	for _, e := range res.GetItems() {
		types = append(types, e.GetType().String())
		if e.GetType() == pb.EventType_EVENT_TYPE_JOB_FALLBACK {
			found = true
			if e.GetRef() == "" {
				t.Errorf("the fallback event names no reference")
			}
		}
	}
	if !found {
		t.Errorf("no EVENT_TYPE_JOB_FALLBACK in the audit log; recorded: %v", types)
	}
}
