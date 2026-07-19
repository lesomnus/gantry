package verify

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
)

func newRefresherTest(t *testing.T, now *time.Time, vfn func() (Result, error)) (*Refresher, *Cache, *spyService) {
	t.Helper()
	cache, err := OpenCache(filepath.Join(t.TempDir(), "v.db"), 28*24*time.Hour, 14*24*time.Hour,
		WithNow(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	spy := &spyService{fn: func(config.StoreConfig, name.Reference) (Result, error) { return vfn() }}
	resolve := func(sourceRef string) (config.StoreConfig, name.Reference, bool) {
		r, err := name.ParseReference(sourceRef, name.Insecure)
		if err != nil {
			return config.StoreConfig{}, nil, false
		}
		return config.StoreConfig{Kind: "oci", Host: r.Context().RegistryStr(), Insecure: true}, r, true
	}
	r := NewRefresher(cache, spy, resolve, RefresherWithNow(func() time.Time { return *now }))
	return r, cache, spy
}

func TestRefresherRenewsStaleTrusted(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("a")
	src := "reg.example/app@" + h.String()
	r, cache, spy := newRefresherTest(t, &now, func() (Result, error) {
		return Result{Mode: config.VerifyRequire, Digest: h}, nil
	})
	_ = cache.Put(h.String(), true, config.VerifyRequire, src)
	now = now.Add(15 * 24 * time.Hour) // stale (past 14d) but not expired (< 28d)

	r.Sweep(context.Background())
	if spy.calls != 1 {
		t.Fatalf("stale entry should be re-verified once, got %d", spy.calls)
	}
	v, _, _ := cache.Get(h.String())
	if !v.Trusted || !v.RefreshAfter.Equal(now.Add(14*24*time.Hour)) {
		t.Errorf("verdict not renewed: %+v", v)
	}
}

func TestRefresherSkipsFresh(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("b")
	r, cache, spy := newRefresherTest(t, &now, func() (Result, error) { return Result{}, nil })
	_ = cache.Put(h.String(), true, config.VerifyRequire, "reg.example/app@"+h.String())
	now = now.Add(1 * 24 * time.Hour) // still fresh
	r.Sweep(context.Background())
	if spy.calls != 0 {
		t.Errorf("a fresh verdict must not be re-verified, calls=%d", spy.calls)
	}
}

func TestRefresherFlipsRevoked(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("c")
	r, cache, _ := newRefresherTest(t, &now, func() (Result, error) { return Result{}, ErrUntrusted })
	_ = cache.Put(h.String(), true, config.VerifyRequire, "reg.example/app@"+h.String())
	now = now.Add(15 * 24 * time.Hour)
	r.Sweep(context.Background())
	v, _, _ := cache.Get(h.String())
	if v.Trusted {
		t.Error("a revoked signature should flip the verdict to untrusted")
	}
}

func TestRefresherLeavesEntryOnOutage(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("d")
	src := "reg.example/app@" + h.String()
	r, cache, _ := newRefresherTest(t, &now, func() (Result, error) {
		return Result{}, errors.New("dial tcp: connection refused")
	})
	_ = cache.Put(h.String(), true, config.VerifyRequire, src)
	before, _, _ := cache.Get(h.String())
	now = now.Add(15 * 24 * time.Hour)
	r.Sweep(context.Background())
	after, found, _ := cache.Get(h.String())
	if !found || !after.Trusted || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("an unreachable registry must leave the entry untouched: before=%+v after=%+v", before, after)
	}
}

// countingVerifier is a concurrency-safe raw verifier for the Run test.
type countingVerifier struct {
	n   atomic.Int64
	res Result
	err error
}

func (c *countingVerifier) Verify(context.Context, config.StoreConfig, name.Reference) (Result, error) {
	c.n.Add(1)
	return c.res, c.err
}

func TestRefresherRunSweepsOnTick(t *testing.T) {
	// Real clock, tiny refresh window so an entry goes stale and the ticker fires
	// quickly; the production entry point Run drives Sweep.
	cache, err := OpenCache(filepath.Join(t.TempDir(), "v.db"), time.Hour, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	h := hashOf("a")
	_ = cache.Put(h.String(), true, config.VerifyRequire, "reg.example/app@"+h.String())

	cv := &countingVerifier{res: Result{Mode: config.VerifyRequire, Digest: h}}
	resolve := func(sourceRef string) (config.StoreConfig, name.Reference, bool) {
		r, err := name.ParseReference(sourceRef, name.Insecure)
		if err != nil {
			return config.StoreConfig{}, nil, false
		}
		return config.StoreConfig{Kind: "oci", Host: r.Context().RegistryStr(), Insecure: true}, r, true
	}
	r := NewRefresher(cache, cv, resolve)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for cv.n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cv.n.Load() == 0 {
		t.Fatal("Run did not re-verify the stale entry")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}

func TestRefresherSkipsTagSource(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	h := hashOf("e")
	r, cache, spy := newRefresherTest(t, &now, func() (Result, error) { return Result{}, nil })
	// SourceRef is a TAG, which could drift — must not be refreshed.
	_ = cache.Put(h.String(), true, config.VerifyRequire, "reg.example/app:1")
	now = now.Add(15 * 24 * time.Hour)
	r.Sweep(context.Background())
	if spy.calls != 0 {
		t.Errorf("a tag-sourced verdict must not be refreshed, calls=%d", spy.calls)
	}
}
