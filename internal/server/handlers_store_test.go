package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/warm"
)

// fakeDockerDaemon answers just enough of the Engine API for a pull to succeed.
func fakeDockerDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("API-Version", "1.44")
			return
		}
		if strings.Contains(r.URL.Path, "/images/create") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{\"status\":\"Pull complete\",\"id\":\"abc\"}\n"))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"Image":"cache.local/team/app:1","ImageID":"sha256:abc"}]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStorePullStampsRetentionIndex(t *testing.T) {
	daemon := fakeDockerDaemon(t)

	var c config.Config
	c.Stores = map[string]config.StoreConfig{
		"eng": {Kind: "docker", Address: "tcp://" + daemon.Listener.Addr().String()},
	}
	c.Worker = config.WorkerConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Stores, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { set.Close() })

	ix, err := retention.Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	gc := retention.NewManager(ix, set.Engines(), retention.Policy{}, retention.Schedule{})

	js := warm.NewMemStore()
	wmr := warm.NewWarmer(set, js, c.Worker)
	hc := health.NewChecker(set, health.Options{})
	h := New(wmr, js, set, gc, hc, nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/store/eng/pull", strings.NewReader(`{"ref":"cache.local/team/app:1"}`))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
	}

	recs, err := ix.List("eng")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Ref != "cache.local/team/app:1" {
		t.Fatalf("records = %+v, want one for the pulled ref", recs)
	}
	if recs[0].LastDistributed.IsZero() {
		t.Error("LastDistributed should be stamped by a manual pull")
	}
}

func TestPinAPIPatterns(t *testing.T) {
	daemon := fakeDockerDaemon(t)

	var c config.Config
	c.Stores = map[string]config.StoreConfig{
		"eng": {Kind: "docker", Address: "tcp://" + daemon.Listener.Addr().String()},
	}
	c.Worker = config.WorkerConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Stores, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { set.Close() })
	ix, err := retention.Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	gc := retention.NewManager(ix, set.Engines(), retention.Policy{}, retention.Schedule{})
	h := New(warm.NewWarmer(set, warm.NewMemStore(), c.Worker), warm.NewMemStore(), set, gc, health.NewChecker(set, health.Options{}), nil, nil)

	do := func(method, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/store/eng/pin", strings.NewReader(body))
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do(http.MethodPost, `{"pattern":"*:stable"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("pin pattern: code = %d, body = %s", rr.Code, rr.Body)
	}
	if rr := do(http.MethodPost, `{"ref":"a:1","pattern":"b:*"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("both ref and pattern must be rejected: %d", rr.Code)
	}
	if rr := do(http.MethodPost, `{}`); rr.Code != http.StatusBadRequest {
		t.Errorf("empty body must be rejected: %d", rr.Code)
	}
	rr := do(http.MethodGet, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: code = %d", rr.Code)
	}
	var listed struct {
		Pins []retention.PinEntry `json:"pins"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Pins) != 1 || listed.Pins[0].Value != "*:stable" || listed.Pins[0].At.IsZero() {
		t.Fatalf("pins = %+v, want one timestamped *:stable entry", listed.Pins)
	}
	if rr := do(http.MethodDelete, `{"pattern":"*:stable"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("unpin: code = %d", rr.Code)
	}
}

func TestPinAPIRejectsInvalidPattern(t *testing.T) {
	daemon := fakeDockerDaemon(t)

	var c config.Config
	c.Stores = map[string]config.StoreConfig{
		"eng": {Kind: "docker", Address: "tcp://" + daemon.Listener.Addr().String()},
	}
	c.Worker = config.WorkerConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Stores, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { set.Close() })
	ix, err := retention.Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	gc := retention.NewManager(ix, set.Engines(), retention.Policy{}, retention.Schedule{})
	h := New(warm.NewWarmer(set, warm.NewMemStore(), c.Worker), warm.NewMemStore(), set, gc, health.NewChecker(set, health.Options{}), nil, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/store/eng/pin", strings.NewReader(`{"pattern":"[unclosed"}`))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("malformed pattern must be rejected, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/store/eng/gc", strings.NewReader(`{"pins":["[unclosed"]}`))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("malformed gc pin override must be rejected, got %d", rr.Code)
	}
}
