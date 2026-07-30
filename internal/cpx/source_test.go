package cpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
)

type testSink struct {
	bytes atomic.Int64
	state atomic.Value
}

func (s *testSink) Add(n int64)        { s.bytes.Add(n) }
func (s *testSink) SetState(st string) { s.state.Store(st) }
func (s *testSink) lastState() string {
	v, _ := s.state.Load().(string)
	return v
}

func startRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// startReferrersRegistry serves a registry with the OCI referrers API enabled
// (like zot), so oras exercises the API path instead of the fallback tag.
func startReferrersRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func pushImage(t *testing.T, ref string, layers int) name.Reference {
	t.Helper()
	img, err := random.Image(512, int64(layers))
	if err != nil {
		t.Fatal(err)
	}
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	return r
}

func pushIndex(t *testing.T, ref string, platforms ...string) name.Reference {
	t.Helper()
	idx := v1.ImageIndex(empty.Index)
	for _, p := range platforms {
		img, err := random.Image(512, 2)
		if err != nil {
			t.Fatal(err)
		}
		plat, err := v1.ParsePlatform(p)
		if err != nil {
			t.Fatal(err)
		}
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: plat}})
	}
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(r, idx); err != nil {
		t.Fatal(err)
	}
	return r
}

func reg(name, host string) config.StoreConfig {
	return config.StoreConfig{Name: name, Kind: "oci", Host: host, Insecure: true, Mode: "copy"}
}

func TestCopySourceFillCommitAndDedup(t *testing.T) {
	ctx := context.Background()
	// Upstream and cache are distinct registries; ggcr's in-memory registry
	// shares blobs globally by digest, so a single host would make every blob
	// look already-present in the cache.
	up := startRegistry(t)
	host := startRegistry(t)
	src := pushImage(t, up+"/src/app:1", 3)
	dst, err := name.ParseReference(host+"/cache/app:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSource(reg("source", up), reg("target", host))
	if err != nil {
		t.Fatal(err)
	}

	plan, err := s.Resolve(ctx, src, dst, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Layers) != 4 { // 3 layers + 1 config
		t.Fatalf("plan layers = %d, want 4", len(plan.Layers))
	}

	var moved int64
	for _, l := range plan.Layers {
		sink := &testSink{}
		if err := s.Fill(ctx, src.Context(), dst.Context(), l, sink); err != nil {
			t.Fatalf("fill %s: %v", l.Digest, err)
		}
		if sink.lastState() != "copied" {
			t.Errorf("state = %q, want copied", sink.lastState())
		}
		moved += sink.bytes.Load()
	}
	if moved != plan.Total {
		t.Errorf("moved %d bytes, want plan total %d", moved, plan.Total)
	}

	if _, err := s.Commit(ctx, src, dst, nil, false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := remote.Get(dst); err != nil {
		t.Errorf("cache tag not resolvable after commit: %v", err)
	}

	var again int64
	for _, l := range plan.Layers {
		sink := &testSink{}
		if err := s.Fill(ctx, src.Context(), dst.Context(), l, sink); err != nil {
			t.Fatalf("re-fill: %v", err)
		}
		if sink.lastState() != "exists" {
			t.Errorf("re-fill state = %q, want exists", sink.lastState())
		}
		again += sink.bytes.Load()
	}
	if again != 0 {
		t.Errorf("re-fill moved %d bytes, want 0", again)
	}
}

func TestCopySourceResolveFiltersPlatforms(t *testing.T) {
	ctx := context.Background()
	host := startRegistry(t)
	src := pushIndex(t, host+"/src/multi:1", "linux/amd64", "linux/arm64")
	dst, err := name.ParseReference(host+"/cache/multi:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSource(reg("source", host), reg("target", host))
	if err != nil {
		t.Fatal(err)
	}

	all, err := s.Resolve(ctx, src, dst, nil)
	if err != nil {
		t.Fatalf("resolve all: %v", err)
	}
	if len(all.Layers) != 6 {
		t.Errorf("all layers = %d, want 6", len(all.Layers))
	}
	one, err := s.Resolve(ctx, src, dst, []string{"linux/arm64"})
	if err != nil {
		t.Fatalf("resolve arm64: %v", err)
	}
	if len(one.Layers) != 3 {
		t.Errorf("arm64 layers = %d, want 3", len(one.Layers))
	}
}

func TestProxySourceReadsThrough(t *testing.T) {
	ctx := context.Background()
	host := startRegistry(t)
	cache := pushImage(t, host+"/cache/app:1", 3)
	s, err := NewSource(config.StoreConfig{}, config.StoreConfig{Name: "target", Kind: "oci", Host: host, Insecure: true, Mode: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.Resolve(ctx, nil, cache, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var read int64
	for _, l := range plan.Layers {
		sink := &testSink{}
		if err := s.Fill(ctx, name.Repository{}, cache.Context(), l, sink); err != nil {
			t.Fatalf("fill: %v", err)
		}
		read += sink.bytes.Load()
	}
	if read != plan.Total {
		t.Errorf("read %d bytes, want plan total %d", read, plan.Total)
	}
}

func TestNewSourceUnknownMode(t *testing.T) {
	if _, err := NewSource(config.StoreConfig{}, config.StoreConfig{Name: "target", Kind: "oci", Mode: "bogus"}); err == nil {
		t.Error("expected error for unknown mode")
	}
}

// startReadOnlyRegistry serves reads normally and refuses every write, like a
// shared registry gantry has pull access to and no push access.
func startReadOnlyRegistry(t *testing.T) string {
	t.Helper()
	inner := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			inner.ServeHTTP(w, r)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"push access denied"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
