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
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/warm"
)

func newTestServer(t *testing.T) (http.Handler, warm.Store) {
	t.Helper()
	var c config.Config
	c.Serve.AllowUnknownStores = true
	c.Serve.Stores = map[string]config.StoreConfig{"cache": {Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"}}
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
	// Start with an already-canceled context so submitted jobs fail fast without
	// touching the network — these tests only exercise the HTTP layer.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wmr.Start(ctx)
	t.Cleanup(wmr.Stop)
	return New(wmr, js, set), js
}

func TestHealthz(t *testing.T) {
	h, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != 200 || rr.Body.String() != "ok" {
		t.Errorf("healthz = %d %q", rr.Code, rr.Body.String())
	}
}

func TestCreateJobValidation(t *testing.T) {
	h, _ := newTestServer(t)
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
	t.Run("nothing to do", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/job", strings.NewReader(`{"ref":"r.io/x:1","from":"r.io"}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})
}

func TestCreateGetDeleteJob(t *testing.T) {
	h, _ := newTestServer(t)

	rr := httptest.NewRecorder()
	body := `{"ref":"team/app:1","from":"example.com","to":"cache","platforms":["linux/amd64"]}`
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/job", strings.NewReader(body)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create code = %d, want 202 (%s)", rr.Code, rr.Body.String())
	}
	var snap warm.JobSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.ID == "" || snap.Ref != "team/app:1" {
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

func TestListJobsFilter(t *testing.T) {
	h, js := newTestServer(t)
	_ = js.Add(warm.NewJob("a", "docker.io/redis:7", nil, time.Now()))
	_ = js.Add(warm.NewJob("b", "ghcr.io/app:1", nil, time.Now()))

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

func TestOpenAPISchema(t *testing.T) {
	h, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/openapi.json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var spec struct {
		OpenAPI string                 `json:"openapi"`
		Paths   map[string]interface{} `json:"paths"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &spec); err != nil {
		t.Fatalf("spec is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", spec.OpenAPI)
	}
	if _, ok := spec.Paths["/v1/job"]; !ok {
		t.Errorf("spec missing /v1/job path; got %v", spec.Paths)
	}
}

func TestListStoresEmpty(t *testing.T) {
	h, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/store", nil))
	var resp struct {
		Items []store.Status `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Name != "cache" || resp.Items[0].Kind != "oci" {
		t.Errorf("stores = %+v", resp.Items)
	}
}
