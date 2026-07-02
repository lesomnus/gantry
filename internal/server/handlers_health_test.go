package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/warm"
)

// TestStoreHealth drives GET /v1/store/{name}/health against a reachable
// registry (200), an unreachable one (503), and an unknown store (404), and
// checks the TTL cache flag.
func TestStoreHealth(t *testing.T) {
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer reg.Close()
	up := strings.TrimPrefix(reg.URL, "http://")

	var c config.Config
	c.Serve.AllowUnknownStores = true
	c.Serve.Stores = map[string]config.StoreConfig{
		"up":   {Kind: "oci", Host: up, Insecure: true},
		"down": {Kind: "oci", Host: "127.0.0.1:1", Insecure: true}, // nothing listens
	}
	c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Serve.Stores, c.Serve.AllowUnknownStores)
	if err != nil {
		t.Fatal(err)
	}
	js := warm.NewMemStore()
	wmr := warm.NewWarmer(set, js, c.Serve.Warm)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wmr.Start(ctx)
	t.Cleanup(wmr.Stop)
	hc := health.NewChecker(set, health.Options{ProbeTimeout: 3 * time.Second})
	h := New(wmr, js, set, nil, hc, nil, nil)

	get := func(name string) (*httptest.ResponseRecorder, health.Report) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/store/"+name+"/health", nil))
		var rep health.Report
		_ = json.Unmarshal(rr.Body.Bytes(), &rep)
		return rr, rep
	}

	t.Run("healthy store -> 200", func(t *testing.T) {
		rr, rep := get("up")
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", rr.Code, rr.Body.String())
		}
		if !rep.Healthy || rep.Name != "up" || rep.Kind != "oci" || rep.Cached {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("cached on second call", func(t *testing.T) {
		_, rep := get("up")
		if !rep.Cached {
			t.Errorf("second call not cached: %+v", rep)
		}
	})
	t.Run("unreachable store -> 503", func(t *testing.T) {
		rr, rep := get("down")
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d, want 503", rr.Code)
		}
		if rep.Healthy || rep.Error == "" {
			t.Errorf("report = %+v, want unhealthy with error", rep)
		}
	})
	t.Run("unknown store -> 404", func(t *testing.T) {
		rr, _ := get("nope")
		if rr.Code != http.StatusNotFound {
			t.Errorf("code = %d, want 404", rr.Code)
		}
	})
}
