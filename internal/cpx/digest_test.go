package cpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
)

// A digest-pinned job may record the image under digest `as` names: the engine
// receives them with the anchor manifest's raw bytes — fetched from the job's
// source, digest-verified — and the retention hook stamps each name.
func TestJobToEngineAsDigest(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, up := engineCopier(t, eng)
	idx := pushIndex(t, up+"/team/app:multi", "linux/amd64", "linux/arm64")
	desc, err := remote.Get(idx)
	if err != nil {
		t.Fatal(err)
	}
	dg := desc.Digest.String()

	var mu sync.Mutex
	var stamped []string
	w.SetPullHook(func(_, ref string) {
		mu.Lock()
		stamped = append(stamped, ref)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	as := []string{"cr.example.com/team/app@" + dg}
	snap, created, err := w.Submit(Request{Ref: "team/app@" + dg, Source: "up", Target: "node", As: as})
	if err != nil || !created {
		t.Fatalf("submit: created=%v err=%v", created, err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}

	eng.mu.Lock()
	gotAs, anchor, digest := eng.as, eng.anchor, eng.digest
	eng.mu.Unlock()
	if len(gotAs) != 1 || gotAs[0] != as[0] {
		t.Errorf("engine as = %v, want %v", gotAs, as)
	}
	if digest != dg {
		t.Errorf("pull digest = %q, want %q", digest, dg)
	}
	if anchor == nil {
		t.Fatal("engine must receive the anchor bytes")
	}
	if anchor.Digest != dg {
		t.Errorf("anchor digest = %q, want %q", anchor.Digest, dg)
	}
	sum := sha256.Sum256(anchor.Bytes)
	if "sha256:"+hex.EncodeToString(sum[:]) != dg {
		t.Error("anchor bytes do not hash to the anchor digest")
	}
	if anchor.MediaType == "" {
		t.Error("anchor media type missing")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stamped) != 1 || stamped[0] != as[0] {
		t.Errorf("retention stamps = %v, want %v", stamped, as)
	}
}

// A digest `as` name is honest only for a digest-pinned job carrying the same
// digest; a tag `as` on a digest job needs no anchor bytes.
func TestAsDigestValidation(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, up := engineCopier(t, eng)
	idx := pushIndex(t, up+"/team/app:multi", "linux/amd64")
	desc, err := remote.Get(idx)
	if err != nil {
		t.Fatal(err)
	}
	dg := desc.Digest.String()
	other := "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	// Unanchored (tag ref, no verification): nothing pins what the name would mean.
	w.SetBaseContext(context.Background())
	_, _, err = w.Submit(Request{Ref: "team/app:multi", Source: "up", Target: "node", As: []string{"cr.example.com/team/app@" + dg}})
	if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Errorf("digest as on an unanchored job must be rejected, got %v", err)
	}

	// The name must carry the job's pinned digest.
	_, _, err = w.Submit(Request{Ref: "team/app@" + dg, Source: "up", Target: "node", As: []string{"cr.example.com/team/app@" + other}})
	if err == nil || !strings.Contains(err.Error(), "pinned digest") {
		t.Errorf("foreign digest as must be rejected, got %v", err)
	}

	// A tag `as` on a digest job stays anchor-free: no bytes are fetched.
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })
	snap, _, err := w.Submit(Request{Ref: "team/app@" + dg, Source: "up", Target: "node", As: []string{"cr.example.com/team/app:multi"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.anchor != nil {
		t.Error("tag-only as must not fetch anchor bytes")
	}
}

// A digest-pinned copy into a registry preserves the source index byte-for-byte
// (the cache-side reference IS the source digest), so the digest resolves from
// the cache; narrowing platforms would rebuild the index under a different
// digest and is refused.
func TestJobDigestRefCopyVerbatim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	up := startRegistry(t)
	cache := startRegistry(t)
	idx := pushIndex(t, up+"/team/app:multi", "linux/amd64", "linux/arm64")
	desc, err := remote.Get(idx)
	if err != nil {
		t.Fatal(err)
	}
	dg := desc.Digest.String()

	w, js := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: up, Insecure: true},
		{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"},
	}, false)

	_, _, err = w.Submit(Request{Ref: "team/app@" + dg, Source: "up", Target: "cache", Platforms: []string{"linux/amd64"}})
	if err == nil || !strings.Contains(err.Error(), "verbatim") {
		t.Fatalf("digest copy with narrowed platforms must be rejected, got %v", err)
	}

	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })
	snap, _, err := w.Submit(Request{Ref: "team/app@" + dg, Source: "up", Target: "cache"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if tr := done.Transfers[0]; tr.Digest != dg {
		t.Errorf("committed digest = %q, want %q", tr.Digest, dg)
	}

	// The source digest resolves from the cache — ggcr verifies the bytes.
	cacheRef, err := name.ParseReference(cache+"/team/app@"+dg, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Get(cacheRef)
	if err != nil {
		t.Fatalf("cache does not resolve the source digest: %v", err)
	}
	if got.Digest.String() != dg {
		t.Errorf("cache digest = %s, want %s", got.Digest, dg)
	}
}

// The retention hook stamps what the engine reports as recorded, not what was
// requested: a classic-store docker skips digest names, and stamping a skipped
// name would leave the image unowned while the index claims it is tracked.
func TestAsDigestClassicSkipStampsReality(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, up := engineCopier(t, eng)
	idx := pushIndex(t, up+"/team/app:multi", "linux/amd64")
	desc, err := remote.Get(idx)
	if err != nil {
		t.Fatal(err)
	}
	dg := desc.Digest.String()
	pullRef := up + "/team/app@" + dg
	eng.recorded = []string{pullRef} // canned: the engine skipped the digest name

	var mu sync.Mutex
	var stamped []string
	w.SetPullHook(func(_, ref string) {
		mu.Lock()
		stamped = append(stamped, ref)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app@" + dg, Source: "up", Target: "node", As: []string{"cr.example.com/team/app@" + dg}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stamped) != 1 || stamped[0] != pullRef {
		t.Errorf("retention stamps = %v, want the daemon's real reference %q", stamped, pullRef)
	}
}
