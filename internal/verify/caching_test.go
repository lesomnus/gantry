package verify

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
)

// spyService is a verify.Service whose Verify is programmable and counts calls.
type spyService struct {
	calls int
	fn    func(from config.StoreConfig, src name.Reference) (Result, error)
	desc  Description
	relod int
}

func (s *spyService) Verify(_ context.Context, from config.StoreConfig, src name.Reference) (Result, error) {
	s.calls++
	return s.fn(from, src)
}
func (s *spyService) Describe() Description        { return s.desc }
func (s *spyService) Reload() (Description, error) { s.relod++; return s.desc, nil }

func hashOf(hex string) v1.Hash {
	h, _ := v1.NewHash("sha256:" + strings.Repeat(hex, 64/len(hex)))
	return h
}

func digestRef(t *testing.T, h v1.Hash) name.Digest {
	t.Helper()
	d, err := name.NewDigest("reg.example/app@"+h.String(), name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newCachingUnderTest(t *testing.T, mode config.VerifyMode, spy *spyService, now *time.Time) (*Caching, *Cache) {
	t.Helper()
	cache, err := OpenCache(filepath.Join(t.TempDir(), "v.db"), 28*24*time.Hour, 14*24*time.Hour,
		WithNow(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	cfg := config.VerifyConfig{Mode: mode}
	c := NewCaching(spy, cache, cfg, CachingWithNow(func() time.Time { return *now }))
	return c, cache
}

var storeUp = config.StoreConfig{Name: "up", Kind: "oci", Host: "reg.example"}

func TestCachingServesTrustedDigestHitOffline(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("a")
	spy := &spyService{fn: func(config.StoreConfig, name.Reference) (Result, error) {
		t.Fatal("inner verifier must NOT be called on a fresh cached hit")
		return Result{}, nil
	}}
	c, cache := newCachingUnderTest(t, config.VerifyRequire, spy, &now)
	if err := cache.Put(h.String(), true, config.VerifyRequire, "reg.example/app@"+h.String()); err != nil {
		t.Fatal(err)
	}
	got, err := c.Verify(context.Background(), storeUp, digestRef(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest.String() != h.String() || spy.calls != 0 {
		t.Fatalf("hit not served offline: digest=%s calls=%d", got.Digest, spy.calls)
	}
}

func TestCachingPopulatesFromTagVerify(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("b")
	spy := &spyService{fn: func(_ config.StoreConfig, _ name.Reference) (Result, error) {
		return Result{Mode: config.VerifyRequire, Digest: h}, nil
	}}
	c, cache := newCachingUnderTest(t, config.VerifyRequire, spy, &now)

	tag, err := name.ParseReference("reg.example/app:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(context.Background(), storeUp, tag); err != nil {
		t.Fatal(err)
	}
	// the resolved digest is now cached as trusted
	v, found, err := cache.Get(h.String())
	if err != nil || !found || !v.Trusted {
		t.Fatalf("tag verify did not populate digest verdict: found=%v v=%+v", found, v)
	}
}

func TestCachingUntrustedDigestCachedTagNotCached(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	hd := hashOf("c")
	c, cache := newCachingUnderTest(t, config.VerifyRequire, &spyService{
		fn: func(_ config.StoreConfig, src name.Reference) (Result, error) {
			return Result{Mode: config.VerifyRequire}, ErrUntrusted
		},
	}, &now)

	// digest source: untrusted verdict is cached
	if _, err := c.Verify(context.Background(), storeUp, digestRef(t, hd)); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("want ErrUntrusted, got %v", err)
	}
	v, found, _ := cache.Get(hd.String())
	if !found || v.Trusted {
		t.Fatalf("digest reject not cached as untrusted: found=%v v=%+v", found, v)
	}

	// tag source: reject NOT cached (no known digest)
	before, _ := cache.Count()
	tag, _ := name.ParseReference("reg.example/app:2", name.Insecure)
	if _, err := c.Verify(context.Background(), storeUp, tag); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("want ErrUntrusted, got %v", err)
	}
	after, _ := cache.Count()
	if after != before {
		t.Errorf("tag reject should not be cached: count %d -> %d", before, after)
	}
}

func TestCachingNeverCachesTransient(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"not found", ErrNotFound},
		{"unreachable", errors.New("dial tcp 10.0.0.1:5000: connect: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hd := hashOf("d")
			c, cache := newCachingUnderTest(t, config.VerifyRequire, &spyService{
				fn: func(config.StoreConfig, name.Reference) (Result, error) { return Result{}, tc.err },
			}, &now)
			if _, err := c.Verify(context.Background(), storeUp, digestRef(t, hd)); err == nil {
				t.Fatal("expected error to propagate")
			}
			if n, _ := cache.Count(); n != 0 {
				t.Errorf("transient failure must not be cached, count=%d", n)
			}
		})
	}
}

func TestCachingExpiredHitReVerifies(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("e")
	spy := &spyService{fn: func(config.StoreConfig, name.Reference) (Result, error) {
		return Result{Mode: config.VerifyRequire, Digest: h}, nil
	}}
	c, cache := newCachingUnderTest(t, config.VerifyRequire, spy, &now)
	_ = cache.Put(h.String(), true, config.VerifyRequire, "")
	// jump past the hard TTL: the cached verdict is unusable, inner must run
	now = now.Add(29 * 24 * time.Hour)
	if _, err := c.Verify(context.Background(), storeUp, digestRef(t, h)); err != nil {
		t.Fatal(err)
	}
	if spy.calls != 1 {
		t.Errorf("expired hit should re-verify (inner calls = %d, want 1)", spy.calls)
	}
}

func TestCachingModeOffBypasses(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	spy := &spyService{fn: func(config.StoreConfig, name.Reference) (Result, error) {
		t.Fatal("inner must not be called when mode is off")
		return Result{}, nil
	}}
	c, cache := newCachingUnderTest(t, config.VerifyOff, spy, &now)
	got, err := c.Verify(context.Background(), storeUp, digestRef(t, hashOf("f")))
	if err != nil {
		t.Fatal(err)
	}
	if got.Verified() || got.Mode != config.VerifyOff {
		t.Errorf("mode off should return an empty off result, got %+v", got)
	}
	if n, _ := cache.Count(); n != 0 {
		t.Errorf("mode off must not touch the cache, count=%d", n)
	}
}

func TestCachingDelegatesDescribeReload(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	spy := &spyService{desc: Description{Enabled: true, Mode: "require"}}
	c, _ := newCachingUnderTest(t, config.VerifyRequire, spy, &now)
	if c.Describe().Mode != "require" {
		t.Error("Describe should delegate to the embedded service")
	}
	if _, err := c.Reload(); err != nil || spy.relod != 1 {
		t.Errorf("Reload should delegate: reloads=%d err=%v", spy.relod, err)
	}
}

func TestCachingReloadClearsCache(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	spy := &spyService{desc: Description{Enabled: true}}
	c, cache := newCachingUnderTest(t, config.VerifyRequire, spy, &now)
	_ = cache.Put(hashOf("a").String(), true, config.VerifyRequire, "")
	if n, _ := cache.Count(); n != 1 {
		t.Fatalf("precondition: cache should hold 1 verdict, got %d", n)
	}
	// a trust-material rotation must invalidate verdicts made against the old store
	if _, err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	if n, _ := cache.Count(); n != 0 {
		t.Errorf("Reload should have cleared the verdict cache, %d remain", n)
	}
}
