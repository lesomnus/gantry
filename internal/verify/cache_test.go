package verify

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
)

func TestCachePutGetRoundTrip(t *testing.T) {
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	now := base
	c, err := OpenCache(filepath.Join(t.TempDir(), "v.db"), 28*24*time.Hour, 14*24*time.Hour,
		WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	const dg = "sha256:aaaa"
	if err := c.Put(dg, true, config.VerifyRequire, "reg.example/app@sha256:aaaa"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := c.Get(dg)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !v.Trusted || v.Digest != dg || v.SourceRef == "" || v.Mode != config.VerifyRequire {
		t.Fatalf("unexpected verdict: %+v", v)
	}
	if !v.VerifiedAt.Equal(base) {
		t.Errorf("VerifiedAt = %v, want %v", v.VerifiedAt, base)
	}
	if !v.RefreshAfter.Equal(base.Add(14 * 24 * time.Hour)) {
		t.Errorf("RefreshAfter = %v", v.RefreshAfter)
	}
	if !v.ExpiresAt.Equal(base.Add(28 * 24 * time.Hour)) {
		t.Errorf("ExpiresAt = %v", v.ExpiresAt)
	}
}

func TestCacheExpiryBoundaries(t *testing.T) {
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	now := base
	c, err := OpenCache(filepath.Join(t.TempDir(), "v.db"), 28*24*time.Hour, 14*24*time.Hour,
		WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Put("sha256:b", true, config.VerifyRequire, ""); err != nil {
		t.Fatal(err)
	}
	v, _, _ := c.Get("sha256:b")

	// within refresh window: neither stale nor expired
	if v.StaleForRefresh(base.Add(13 * 24 * time.Hour)) {
		t.Error("should not be stale before refresh age")
	}
	if v.Expired(base.Add(13 * 24 * time.Hour)) {
		t.Error("should not be expired before ttl")
	}
	// past soft refresh, before hard ttl: stale but still usable
	if !v.StaleForRefresh(base.Add(15 * 24 * time.Hour)) {
		t.Error("should be stale past refresh age")
	}
	if v.Expired(base.Add(15 * 24 * time.Hour)) {
		t.Error("should not be expired before ttl")
	}
	// past hard ttl
	if !v.Expired(base.Add(29 * 24 * time.Hour)) {
		t.Error("should be expired past ttl")
	}
}

func TestCacheGetMissAndCountAndForEach(t *testing.T) {
	c, err := OpenCache(filepath.Join(t.TempDir(), "v.db"), time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, ok, err := c.Get("sha256:absent"); ok || err != nil {
		t.Fatalf("miss: ok=%v err=%v", ok, err)
	}
	if n, _ := c.Count(); n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
	_ = c.Put("sha256:1", true, config.VerifyRequire, "")
	_ = c.Put("sha256:2", false, config.VerifyRequire, "")
	if n, _ := c.Count(); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	seen := map[string]bool{}
	if err := c.ForEach(func(v Verdict) error { seen[v.Digest] = v.Trusted; return nil }); err != nil {
		t.Fatal(err)
	}
	if !seen["sha256:1"] || seen["sha256:2"] {
		t.Errorf("ForEach verdicts: %+v", seen)
	}
}

func TestCacheDeleteAndOverwrite(t *testing.T) {
	c, err := OpenCache(filepath.Join(t.TempDir(), "v.db"), time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.Put("sha256:x", true, config.VerifyRequire, "")
	// overwrite flips trust
	_ = c.Put("sha256:x", false, config.VerifyRequire, "")
	v, _, _ := c.Get("sha256:x")
	if v.Trusted {
		t.Error("overwrite should have flipped to untrusted")
	}
	if err := c.Delete("sha256:x"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get("sha256:x"); ok {
		t.Error("verdict should be gone after Delete")
	}
	// deleting a missing key is fine
	if err := c.Delete("sha256:missing"); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
}

func TestCachePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.db")
	c, err := OpenCache(path, time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Put("sha256:keep", true, config.VerifyRequire, "reg/app@sha256:keep")
	c.Close()

	c2, err := OpenCache(path, time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	v, ok, err := c2.Get("sha256:keep")
	if err != nil || !ok || !v.Trusted {
		t.Fatalf("verdict did not persist: ok=%v err=%v v=%+v", ok, err, v)
	}
}
