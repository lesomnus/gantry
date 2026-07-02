package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/warm"
)

// newGCServer builds a handler over one fake docker engine store ("eng") with a
// retention manager, mirroring the pin/pull test harness.
func newGCServer(t *testing.T) (http.Handler, *retention.Index) {
	t.Helper()
	daemon := fakeDockerDaemon(t)
	var c config.Config
	c.Serve.Stores = map[string]config.StoreConfig{
		"eng": {Kind: "docker", Address: "tcp://" + daemon.Listener.Addr().String()},
	}
	c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Serve.Stores, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { set.Close() })
	ix, err := retention.Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	gc := retention.NewManager(ix, set.Engines(), retention.Policy{KeepN: 2}, retention.Schedule{})
	h := New(warm.NewWarmer(set, warm.NewMemStore(), c.Serve.Warm), warm.NewMemStore(), set, gc, health.NewChecker(set, health.Options{}), nil)
	return h, ix
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	h.ServeHTTP(rr, req)
	return rr
}

func TestVersionEndpoint(t *testing.T) {
	h, _ := newTestServer(t)
	rr := do(t, h, http.MethodGet, "/v1/version", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	var v struct {
		Version string `json:"version"`
		GitRev  string `json:"git_rev"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version == "" || v.GitRev == "" {
		t.Errorf("version = %+v, want stamped build info", v)
	}
}

func TestGCStatusEndpoint(t *testing.T) {
	t.Run("disabled without retention", func(t *testing.T) {
		h, _ := newTestServer(t) // gc == nil
		if rr := do(t, h, http.MethodGet, "/v1/gc", ""); rr.Code != http.StatusNotImplemented {
			t.Errorf("code = %d, want 501", rr.Code)
		}
	})
	t.Run("reports scheduler and index state", func(t *testing.T) {
		h, ix := newGCServer(t)
		_ = ix.Touch("eng", "cache.local/a/app:1", time.Now())
		_ = ix.Pin("eng", "*:stable", true)
		rr := do(t, h, http.MethodGet, "/v1/gc", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
		}
		var st retention.Status
		if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if st.Enabled {
			t.Error("scheduler with zero interval must report enabled=false (manual GC only)")
		}
		if st.Policy.KeepN != 2 {
			t.Errorf("policy.keep_n = %d, want 2", st.Policy.KeepN)
		}
		if got := st.Stores["eng"]; got.Records != 1 || got.Pins != 1 {
			t.Errorf("stores[eng] = %+v, want 1 record / 1 pin", got)
		}
	})
}

func TestStoreInUseEndpoint(t *testing.T) {
	h, _ := newGCServer(t)
	rr := do(t, h, http.MethodGet, "/v1/store/eng/inuse", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
	}
	var res struct {
		InUse []string `json:"in_use"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	want := []string{"cache.local/team/app:1", "sha256:abc"}
	if len(res.InUse) != 2 || res.InUse[0] != want[0] || res.InUse[1] != want[1] {
		t.Errorf("in_use = %v, want %v", res.InUse, want)
	}
	if rr := do(t, h, http.MethodGet, "/v1/store/nope/inuse", ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown store = %d, want 404", rr.Code)
	}
}

func TestStoreImageInventory(t *testing.T) {
	h, ix := newGCServer(t)
	_ = ix.Touch("eng", "cache.local/a/app:1", time.Now())
	_ = ix.Touch("eng", "cache.local/a/app:2", time.Now())
	_ = ix.Touch("eng", "cache.local/b/other:1", time.Now())
	_ = ix.Pin("eng", "cache.local/a/app:1", false)

	list := func(query string) []retention.Record {
		t.Helper()
		rr := do(t, h, http.MethodGet, "/v1/store/eng/image"+query, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
		}
		var res struct {
			Items []retention.Record `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res.Items
	}
	if items := list(""); len(items) != 3 {
		t.Errorf("items = %d, want 3", len(items))
	}
	if items := list("?pinned=true"); len(items) != 1 || items[0].Ref != "cache.local/a/app:1" {
		t.Errorf("pinned filter = %+v", items)
	}
	if items := list("?repo=" + "cache.local/a/app"); len(items) != 2 {
		t.Errorf("repo filter = %d, want 2", len(items))
	}
	if items := list("?ref=cache.local/b/other:1"); len(items) != 1 {
		t.Errorf("ref filter = %d, want 1", len(items))
	}
	if rr := do(t, h, http.MethodGet, "/v1/store/eng/image?pinned=bogus", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("bad pinned filter = %d, want 400", rr.Code)
	}

	// DELETE purges only the index record — the orphan escape hatch.
	rr := do(t, h, http.MethodDelete, "/v1/store/eng/image", `{"ref":"cache.local/b/other:1"}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, body = %s", rr.Code, rr.Body)
	}
	if items := list(""); len(items) != 2 {
		t.Errorf("items after delete = %d, want 2", len(items))
	}
	if rr := do(t, h, http.MethodDelete, "/v1/store/eng/image", `{"ref":"cache.local/b/other:1"}`); rr.Code != http.StatusNotFound {
		t.Errorf("deleting a missing record = %d, want 404", rr.Code)
	}
}

func TestStoreWatcherEndpoint(t *testing.T) {
	h, _ := newGCServer(t)
	rr := do(t, h, http.MethodGet, "/v1/store/eng/watcher", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
	}
	var ws retention.WatcherStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	if ws.Connected {
		t.Error("watcher not started; connected must be false")
	}
	if rr := do(t, h, http.MethodGet, "/v1/store/nope/watcher", ""); rr.Code != http.StatusNotFound {
		t.Errorf("unknown store = %d, want 404", rr.Code)
	}
}

func TestReadyz(t *testing.T) {
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(reg.Close)

	newH := func(host string, gate []string) http.Handler {
		t.Helper()
		var c config.Config
		c.Serve.Stores = map[string]config.StoreConfig{
			"reg": {Kind: "oci", Host: host, Insecure: true},
		}
		c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 1, MaxConcurrentLayers: 1, QueueSize: 8}
		if err := c.Evaluate(); err != nil {
			t.Fatal(err)
		}
		set, err := store.NewSet(c.Serve.Stores, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { set.Close() })
		hc := health.NewChecker(set, health.Options{ReadyStores: gate, ProbeTimeout: time.Second})
		return New(warm.NewWarmer(set, warm.NewMemStore(), c.Serve.Warm), warm.NewMemStore(), set, nil, hc, nil)
	}
	host := strings.TrimPrefix(reg.URL, "http://")
	t.Run("gated store healthy", func(t *testing.T) {
		// Auth disabled counts as authenticated: the breakdown is included.
		h := Auth(config.AuthConfig{})(newH(host, []string{"reg"}))
		rr := do(t, h, http.MethodGet, "/readyz", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
		}
		var res struct {
			Ready  bool            `json:"ready"`
			Stores []health.Report `json:"stores"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if !res.Ready || len(res.Stores) != 1 || !res.Stores[0].Healthy {
			t.Errorf("res = %+v, want ready with one healthy report", res)
		}
	})
	t.Run("gated store down", func(t *testing.T) {
		rr := do(t, newH("127.0.0.1:1", []string{"reg"}), http.MethodGet, "/readyz", "")
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("code = %d, want 503", rr.Code)
		}
	})
	t.Run("empty gate is trivially ready", func(t *testing.T) {
		// No engine stores and no ready_stores: nothing gates readiness.
		rr := do(t, newH(host, nil), http.MethodGet, "/readyz", "")
		if rr.Code != http.StatusOK {
			t.Errorf("code = %d, want 200", rr.Code)
		}
	})
	t.Run("anonymous callers get the verdict without the breakdown", func(t *testing.T) {
		t.Setenv("GANTRY_TEST_TOKEN", "secret")
		h := Auth(config.AuthConfig{Tokens: []string{"${GANTRY_TEST_TOKEN}"}})(newH(host, []string{"reg"}))
		rr := do(t, h, http.MethodGet, "/readyz", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("anonymous readyz must bypass auth, got %d", rr.Code)
		}
		if strings.Contains(rr.Body.String(), "stores") {
			t.Errorf("anonymous body must omit per-store detail: %s", rr.Body)
		}
		rr = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		req.Header.Set("Authorization", "Bearer secret")
		h.ServeHTTP(rr, req)
		if !strings.Contains(rr.Body.String(), "stores") {
			t.Errorf("authorized body must include the breakdown: %s", rr.Body)
		}
	})
}
