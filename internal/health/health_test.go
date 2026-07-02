package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/otx"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeClock is a manually-advanced clock for deterministic TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newChecker builds a Checker over an oci store named "reg" (no engine dial) with
// a fake clock. The caller overrides ck.probe to control probe behavior.
func newChecker(t *testing.T, ttl time.Duration) (*Checker, *fakeClock) {
	t.Helper()
	set, err := store.NewSet(map[string]config.StoreConfig{
		"reg": {Kind: "oci", Host: "example.test"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	ck := NewChecker(set, Options{CacheTTL: ttl})
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	ck.now = clk.now
	return ck, clk
}

func TestCheckerCachesWithinTTL(t *testing.T) {
	ck, clk := newChecker(t, 5*time.Second)
	var calls int32
	ck.probe = func(context.Context, config.StoreConfig) error {
		atomic.AddInt32(&calls, 1)
		return nil
	}

	r1, err := ck.Check(context.Background(), "reg")
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Healthy || r1.Cached || r1.Name != "reg" || r1.Kind != "oci" {
		t.Fatalf("first = %+v", r1)
	}
	r2, _ := ck.Check(context.Background(), "reg")
	if !r2.Cached {
		t.Errorf("second call should be cached: %+v", r2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("probe calls = %d, want 1 within TTL", got)
	}

	clk.advance(6 * time.Second) // past TTL
	r3, _ := ck.Check(context.Background(), "reg")
	if r3.Cached {
		t.Errorf("post-expiry call should re-probe (not cached): %+v", r3)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("probe calls = %d, want 2 after expiry", got)
	}
}

func TestCheckerReportsUnhealthyNotError(t *testing.T) {
	ck, _ := newChecker(t, 5*time.Second)
	ck.probe = func(context.Context, config.StoreConfig) error {
		return errors.New("daemon down")
	}
	r, err := ck.Check(context.Background(), "reg")
	if err != nil {
		t.Fatalf("Check err = %v, want nil (failure goes in the report)", err)
	}
	if r.Healthy || r.Error != "daemon down" {
		t.Errorf("report = %+v, want unhealthy with error", r)
	}
}

func TestCheckerUnknownStore(t *testing.T) {
	ck, _ := newChecker(t, 5*time.Second)
	if _, err := ck.Check(context.Background(), "nope"); !errors.Is(err, ErrUnknownStore) {
		t.Errorf("err = %v, want ErrUnknownStore", err)
	}
}

// TestCheckerSingleflight verifies concurrent callers for one store coalesce
// onto a single probe rather than stampeding the backend.
func TestCheckerSingleflight(t *testing.T) {
	ck, _ := newChecker(t, time.Hour)
	release := make(chan struct{})
	var calls int32
	ck.probe = func(context.Context, config.StoreConfig) error {
		atomic.AddInt32(&calls, 1)
		<-release // hold the probe so concurrent callers pile up on the entry lock
		return nil
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ck.Check(context.Background(), "reg")
		}()
	}
	// Let the first caller enter the probe; the rest block on the entry mutex.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("probe calls = %d, want 1 (singleflight)", got)
	}
}

// TestPingRegistry probes the real /v2/ ping against an httptest registry.
func TestPingRegistry(t *testing.T) {
	ck := NewChecker(mustSet(t, nil), Options{})
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		healthy bool
	}{
		{"200 open registry", http.StatusOK, nil, true},
		{"401 auth challenge", http.StatusUnauthorized, map[string]string{"WWW-Authenticate": `Bearer realm="x"`}, true},
		{"500 broken", http.StatusInternalServerError, nil, false},
		{"404 not a registry", http.StatusNotFound, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/v2") {
					http.NotFound(w, r)
					return
				}
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			host := strings.TrimPrefix(srv.URL, "http://")
			err := ck.pingRegistry(context.Background(), config.StoreConfig{Name: "reg", Kind: "oci", Host: host, Insecure: true})
			if tc.healthy && err != nil {
				t.Errorf("expected healthy, got err: %v", err)
			}
			if !tc.healthy && err == nil {
				t.Error("expected unhealthy, got nil err")
			}
		})
	}
}

// TestProbeStoreEngineDispatch exercises the engine branch of probeStore offline:
// a docker store at a dead address dials lazily, so Check reaches eng.Ready,
// whose failure must surface as an unhealthy report (not a panic or an error).
func TestProbeStoreEngineDispatch(t *testing.T) {
	set, err := store.NewSet(map[string]config.StoreConfig{
		"eng": {Kind: "docker", Address: "tcp://127.0.0.1:1"}, // nothing listens
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	ck := NewChecker(set, Options{ProbeTimeout: 2 * time.Second})
	r, err := ck.Check(context.Background(), "eng")
	if err != nil {
		t.Fatalf("Check err = %v, want nil", err)
	}
	if r.Kind != "docker" {
		t.Errorf("kind = %q, want docker", r.Kind)
	}
	if r.Healthy || r.Error == "" {
		t.Errorf("report = %+v, want unhealthy engine with error", r)
	}
}

// TestDistinctStoresProbeConcurrently verifies the singleflight lock is per
// store: a blocked probe on store A must not delay a probe on store B.
func TestDistinctStoresProbeConcurrently(t *testing.T) {
	set := mustSet(t, map[string]config.StoreConfig{
		"a": {Kind: "oci", Host: "a.test"},
		"b": {Kind: "oci", Host: "b.test"},
	})
	ck := NewChecker(set, Options{})
	release := make(chan struct{})
	ck.probe = func(_ context.Context, c config.StoreConfig) error {
		if c.Name == "a" {
			<-release // hold A indefinitely
		}
		return nil
	}

	// Block A in a goroutine, then B must still complete promptly on its own lock.
	go func() { _, _ = ck.Check(context.Background(), "a") }()
	done := make(chan struct{})
	go func() { _, _ = ck.Check(context.Background(), "b"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Check(b) blocked behind Check(a): probes are not per-store independent")
	}
	close(release)
}

// mustSet builds a store.Set of oci stores (no engine dialing) for tests.
func mustSet(t *testing.T, stores map[string]config.StoreConfig) *store.Set {
	t.Helper()
	if stores == nil {
		stores = map[string]config.StoreConfig{"reg": {Kind: "oci", Host: "example.test"}}
	}
	set, err := store.NewSet(stores, false)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// TestProbeStoreRegistryDispatch exercises Check -> runProbe -> probeStore ->
// pingRegistry end-to-end (no injected probe) against a live httptest registry.
func TestProbeStoreRegistryDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	set, err := store.NewSet(map[string]config.StoreConfig{
		"reg": {Kind: "oci", Host: host, Insecure: true},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	ck := NewChecker(set, Options{})
	r, err := ck.Check(context.Background(), "reg")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Healthy {
		t.Errorf("registry dispatch = %+v, want healthy", r)
	}
	if r.LatencyMS < 0 {
		t.Errorf("latency = %d", r.LatencyMS)
	}
}

func TestProbeDurationRecorded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { mp.Shutdown(context.Background()) })
	ctx := otx.Into(context.Background(), otx.New(otx.WithMeterProvider(mp)))

	ck, _ := newChecker(t, time.Second)
	ck.probe = func(context.Context, config.StoreConfig) error { return nil }
	if _, err := ck.Check(ctx, "reg"); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "gantry.health.probe.duration" {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("data = %T, want float64 histogram", m.Data)
			}
			if len(h.DataPoints) != 1 || h.DataPoints[0].Count != 1 {
				t.Fatalf("datapoints = %+v, want one recording", h.DataPoints)
			}
			return
		}
	}
	t.Fatal("probe duration histogram not recorded")
}
