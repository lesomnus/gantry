package warm

import (
	"context"
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

func TestCopySourceWarmCommitAndDedup(t *testing.T) {
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
	s, err := NewSource(reg("from", up), reg("to", host))
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
		if err := s.Warm(ctx, src.Context(), dst.Context(), l, sink); err != nil {
			t.Fatalf("warm %s: %v", l.Digest, err)
		}
		if sink.lastState() != "warm" {
			t.Errorf("state = %q, want warm", sink.lastState())
		}
		moved += sink.bytes.Load()
	}
	if moved != plan.Total {
		t.Errorf("moved %d bytes, want plan total %d", moved, plan.Total)
	}

	if err := s.Commit(ctx, src, dst, nil); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := remote.Get(dst); err != nil {
		t.Errorf("cache tag not resolvable after commit: %v", err)
	}

	var again int64
	for _, l := range plan.Layers {
		sink := &testSink{}
		if err := s.Warm(ctx, src.Context(), dst.Context(), l, sink); err != nil {
			t.Fatalf("re-warm: %v", err)
		}
		if sink.lastState() != "exists" {
			t.Errorf("re-warm state = %q, want exists", sink.lastState())
		}
		again += sink.bytes.Load()
	}
	if again != 0 {
		t.Errorf("re-warm moved %d bytes, want 0", again)
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
	s, err := NewSource(reg("from", host), reg("to", host))
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
	s, err := NewSource(config.StoreConfig{}, config.StoreConfig{Name: "to", Kind: "oci", Host: host, Insecure: true, Mode: "proxy"})
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
		if err := s.Warm(ctx, name.Repository{}, cache.Context(), l, sink); err != nil {
			t.Fatalf("warm: %v", err)
		}
		read += sink.bytes.Load()
	}
	if read != plan.Total {
		t.Errorf("read %d bytes, want plan total %d", read, plan.Total)
	}
}

func TestNewSourceUnknownMode(t *testing.T) {
	if _, err := NewSource(config.StoreConfig{}, config.StoreConfig{Name: "to", Kind: "oci", Mode: "bogus"}); err == nil {
		t.Error("expected error for unknown mode")
	}
}
