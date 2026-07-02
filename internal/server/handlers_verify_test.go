package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
)

// fakeVerifyService is a canned verify.Service.
type fakeVerifyService struct {
	res     verify.Result
	err     error
	desc    verify.Description
	rel_err error
	reloads int
	seen    string
}

func (f *fakeVerifyService) Verify(_ context.Context, _ config.StoreConfig, src name.Reference) (verify.Result, error) {
	f.seen = src.Name()
	return f.res, f.err
}

func (f *fakeVerifyService) Describe() verify.Description { return f.desc }

func (f *fakeVerifyService) Reload() (verify.Description, error) {
	f.reloads++
	return f.desc, f.rel_err
}

func newVerifyServer(t *testing.T, vf verify.Service) http.Handler {
	t.Helper()
	var c config.Config
	c.Serve.Stores = map[string]config.StoreConfig{
		"up":   {Kind: "oci", Host: "up.example", Insecure: true},
		"open": {Kind: "oci", Host: "open.example", Verify: &config.StoreVerify{Mode: config.VerifyOff}},
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
	return New(warm.NewWarmer(set, warm.NewMemStore(), c.Serve.Warm), warm.NewMemStore(), set, nil, health.NewChecker(set, health.Options{}), vf, nil)
}

func TestVerifyPreflight(t *testing.T) {
	h, _ := v1.NewHash("sha256:" + strings.Repeat("d", 64))
	t.Run("verified", func(t *testing.T) {
		fv := &fakeVerifyService{res: verify.Result{Mode: config.VerifyRequire, Digest: h}}
		srv := newVerifyServer(t, fv)
		rr := do(t, srv, http.MethodPost, "/v1/verify", `{"ref":"app/x:1","from":"up"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
		}
		var res struct {
			Ref      string `json:"ref"`
			Mode     string `json:"mode"`
			Verified bool   `json:"verified"`
			Digest   string `json:"digest"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if !res.Verified || res.Digest != h.String() || res.Mode != "require" {
			t.Errorf("res = %+v", res)
		}
		if fv.seen != "up.example/app/x:1" {
			t.Errorf("verified ref = %q, want the source-store resolved ref", fv.seen)
		}
	})
	t.Run("unsigned is 422", func(t *testing.T) {
		fv := &fakeVerifyService{err: fmt.Errorf("%w: app/x:1", verify.ErrUnsigned)}
		rr := do(t, newVerifyServer(t, fv), http.MethodPost, "/v1/verify", `{"ref":"app/x:1","from":"up"}`)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("code = %d, want 422", rr.Code)
		}
	})
	t.Run("transport failure is 502", func(t *testing.T) {
		fv := &fakeVerifyService{err: fmt.Errorf("dial tcp: connection refused")}
		rr := do(t, newVerifyServer(t, fv), http.MethodPost, "/v1/verify", `{"ref":"app/x:1","from":"up"}`)
		if rr.Code != http.StatusBadGateway {
			t.Errorf("code = %d, want 502", rr.Code)
		}
	})
	t.Run("unknown store is 400", func(t *testing.T) {
		rr := do(t, newVerifyServer(t, &fakeVerifyService{}), http.MethodPost, "/v1/verify", `{"ref":"app/x:1","from":"nope"}`)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})
	t.Run("disabled is 501", func(t *testing.T) {
		rr := do(t, newVerifyServer(t, nil), http.MethodPost, "/v1/verify", `{"ref":"app/x:1"}`)
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("code = %d, want 501", rr.Code)
		}
	})
	t.Run("typed-nil service means disabled, not a panic", func(t *testing.T) {
		// The production wiring holds a *verify.Swappable; a nil one wrapped in
		// the interface must behave exactly like disabled.
		srv := newVerifyServer(t, (*verify.Swappable)(nil))
		if rr := do(t, srv, http.MethodPost, "/v1/verify", `{"ref":"app/x:1"}`); rr.Code != http.StatusNotImplemented {
			t.Errorf("preflight = %d, want 501", rr.Code)
		}
		if rr := do(t, srv, http.MethodGet, "/v1/verify", ""); rr.Code != http.StatusOK {
			t.Errorf("describe = %d, want 200", rr.Code)
		}
	})
	t.Run("missing reference is 404", func(t *testing.T) {
		fv := &fakeVerifyService{err: fmt.Errorf("%w: app/x:1", verify.ErrNotFound)}
		rr := do(t, newVerifyServer(t, fv), http.MethodPost, "/v1/verify", `{"ref":"app/x:1","from":"up"}`)
		if rr.Code != http.StatusNotFound {
			t.Errorf("code = %d, want 404", rr.Code)
		}
	})
	t.Run("digest ref resolves on the source store", func(t *testing.T) {
		dg := "sha256:" + strings.Repeat("e", 64)
		fv := &fakeVerifyService{res: verify.Result{Mode: config.VerifyRequire}}
		rr := do(t, newVerifyServer(t, fv), http.MethodPost, "/v1/verify", `{"ref":"app/x@`+dg+`","from":"up"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d, body = %s", rr.Code, rr.Body)
		}
		if fv.seen != "up.example/app/x@"+dg {
			t.Errorf("verified ref = %q, want the digest preserved", fv.seen)
		}
	})
}

func TestVerifyDescribe(t *testing.T) {
	t.Run("disabled reports mode off", func(t *testing.T) {
		rr := do(t, newVerifyServer(t, nil), http.MethodGet, "/v1/verify", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), `"enabled":false`) {
			t.Errorf("body = %s", rr.Body)
		}
	})
	t.Run("per-store effective modes", func(t *testing.T) {
		fv := &fakeVerifyService{desc: verify.Description{Enabled: true, Provider: "notation", Mode: "require"}}
		rr := do(t, newVerifyServer(t, fv), http.MethodGet, "/v1/verify", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d", rr.Code)
		}
		var res struct {
			Stores map[string]string `json:"stores"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Stores["up"] != "require" || res.Stores["open"] != "off" {
			t.Errorf("stores = %v, want the per-store override reflected", res.Stores)
		}
		if len(res.Stores) != 2 {
			t.Errorf("stores = %v, want registry stores only", res.Stores)
		}
	})
}

func TestVerifyReload(t *testing.T) {
	t.Run("swaps and reports", func(t *testing.T) {
		fv := &fakeVerifyService{desc: verify.Description{Enabled: true, Mode: "require"}}
		rr := do(t, newVerifyServer(t, fv), http.MethodPost, "/v1/verify/reload", "")
		if rr.Code != http.StatusOK || fv.reloads != 1 {
			t.Errorf("code = %d reloads = %d", rr.Code, fv.reloads)
		}
	})
	t.Run("rejected material is 422", func(t *testing.T) {
		fv := &fakeVerifyService{rel_err: fmt.Errorf("%w: no CA certificates found", verify.ErrBadTrustMaterial)}
		rr := do(t, newVerifyServer(t, fv), http.MethodPost, "/v1/verify/reload", "")
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("code = %d, want 422", rr.Code)
		}
	})
	t.Run("transient failure is 500", func(t *testing.T) {
		fv := &fakeVerifyService{rel_err: fmt.Errorf("read trust_store dir: input/output error")}
		rr := do(t, newVerifyServer(t, fv), http.MethodPost, "/v1/verify/reload", "")
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("code = %d, want 500", rr.Code)
		}
	})
	t.Run("disabled is 501", func(t *testing.T) {
		rr := do(t, newVerifyServer(t, nil), http.MethodPost, "/v1/verify/reload", "")
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("code = %d, want 501", rr.Code)
		}
	})
}
