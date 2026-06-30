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
	"github.com/lesomnus/gantry/internal/warm"
)

func newTestServer(t *testing.T) (http.Handler, warm.Store, *warm.Warmer) {
	t.Helper()
	var c config.Config
	c.Serve.Registry = config.RegistryConfig{Mode: "copy", Host: "cache.local", Insecure: true}
	c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	src, err := warm.NewSource(c.Serve.Registry)
	if err != nil {
		t.Fatal(err)
	}
	store := warm.NewMemStore()
	wmr := warm.NewWarmer(src, store, c.Serve.Registry, c.Serve.Warm)
	wmr.Start(context.Background())
	t.Cleanup(wmr.Stop)
	return New(wmr, store, nil), store, wmr
}

func TestHealthz(t *testing.T) {
	h, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != 200 || rr.Body.String() != "ok" {
		t.Errorf("healthz = %d %q", rr.Code, rr.Body.String())
	}
}

func TestCreateWarmValidation(t *testing.T) {
	h, _, _ := newTestServer(t)
	t.Run("missing ref", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/job", strings.NewReader(`{}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})
	t.Run("unknown field rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/job", strings.NewReader(`{"ref":"x","bogus":1}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})
}

func TestCreateGetDeleteWarm(t *testing.T) {
	h, _, _ := newTestServer(t)

	rr := httptest.NewRecorder()
	body := `{"ref":"example.com/team/app:1","platforms":["linux/amd64"]}`
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/job", strings.NewReader(body)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create code = %d, want 202 (%s)", rr.Code, rr.Body.String())
	}
	var snap warm.JobSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ID == "" || snap.Ref != "example.com/team/app:1" {
		t.Fatalf("snap = %+v", snap)
	}
	if loc := rr.Header().Get("Location"); loc != "/v1/job/"+snap.ID {
		t.Errorf("Location = %q", loc)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/job/"+snap.ID, nil))
	if rr.Code != http.StatusOK {
		t.Errorf("get code = %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/job/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("get missing code = %d, want 404", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("DELETE", "/v1/job/"+snap.ID, nil))
	if rr.Code != http.StatusNoContent {
		t.Errorf("delete code = %d, want 204", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("DELETE", "/v1/job/"+snap.ID, nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("re-delete code = %d, want 404", rr.Code)
	}
}

func TestListWarmsFilter(t *testing.T) {
	h, store, _ := newTestServer(t)
	a := warm.NewJob("a", "docker.io/redis:7", "cache.local/redis:7", nil, time.Now())
	b := warm.NewJob("b", "ghcr.io/app:1", "cache.local/app:1", nil, time.Now())
	_ = store.Add(a)
	_ = store.Add(b)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/job?ref=ghcr", nil))
	var resp struct {
		Items []warm.JobSnapshot `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "b" {
		t.Errorf("filtered items = %+v", resp.Items)
	}
}
