package cpx

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
)

// attachReferrer packs and pushes a minimal artifact manifest whose subject is
// the given descriptor, like a notation signature. variant makes the artifact
// distinct: the manifest is content-addressed, so two attachments of the same
// bytes are one referrer, not two.
func attachReferrer(t *testing.T, store config.StoreConfig, repo name.Repository, subject *remote.Descriptor, variant ...string) {
	t.Helper()
	ctx := context.Background()
	r, err := orasRepo(store, repo)
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte(`{"sig":"test` + strings.Join(variant, "") + `"}`)
	blob_desc := ocispec.Descriptor{
		MediaType: "application/vnd.test.signature",
		Digest:    godigest.FromBytes(blob),
		Size:      int64(len(blob)),
	}
	if err := r.Push(ctx, blob_desc, bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}
	subject_desc := ocispec.Descriptor{
		MediaType: string(subject.MediaType),
		Digest:    godigest.Digest(subject.Digest.String()),
		Size:      subject.Size,
	}
	_, err = oras.PackManifest(ctx, r, oras.PackManifestVersion1_1, "application/vnd.test.signature",
		oras.PackManifestOptions{
			Subject: &subject_desc,
			Layers:  []ocispec.Descriptor{blob_desc},
		})
	if err != nil {
		t.Fatal(err)
	}
}

func listReferrers(t *testing.T, store config.StoreConfig, repo name.Repository, dg v1.Hash) []ocispec.Descriptor {
	t.Helper()
	ctx := context.Background()
	r, err := orasRepo(store, repo)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := r.Resolve(ctx, dg.String())
	if err != nil {
		t.Fatal(err)
	}
	var out []ocispec.Descriptor
	err = r.Referrers(ctx, desc, "", func(ds []ocispec.Descriptor) error {
		out = append(out, ds...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A copy_referrers job must land the image in the cache at the SOURCE digest
// (verbatim, all platforms) with its referrer artifacts alongside, and anchor
// the recorded transfer to that digest. Runs against both referrers schemes:
// the fallback-tag registry and the OCI referrers API (zot's mode).
func TestJobCopyReferrersVerbatim(t *testing.T) {
	t.Run("fallback tag", func(t *testing.T) { testJobCopyReferrersVerbatim(t, startRegistry) })
	t.Run("referrers API", func(t *testing.T) { testJobCopyReferrersVerbatim(t, startReferrersRegistry) })
}

func testJobCopyReferrersVerbatim(t *testing.T, start func(*testing.T) string) {
	ctx, cancel := context.WithCancel(context.Background())
	up := start(t)
	cache := start(t)
	src_ref := pushIndex(t, up+"/team/app:1", "linux/amd64", "linux/arm64")
	src_desc, err := remote.Get(src_ref)
	if err != nil {
		t.Fatal(err)
	}
	up_store := config.StoreConfig{Name: "up", Kind: "oci", Host: up, Insecure: true}
	cache_store := config.StoreConfig{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"}
	attachReferrer(t, up_store, src_ref.Context(), src_desc)

	w, js := newCopier(t, []config.StoreConfig{up_store, cache_store}, false)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	yes := true
	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "cache", CopyReferrers: &yes})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	tr := done.Transfers[0]
	if tr.Digest != src_desc.Digest.String() {
		t.Errorf("transfer digest = %q, want the source digest %q", tr.Digest, src_desc.Digest)
	}
	dst_ref, err := name.ParseReference(tr.Ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	dst_desc, err := remote.Get(dst_ref)
	if err != nil {
		t.Fatal(err)
	}
	if dst_desc.Digest != src_desc.Digest {
		t.Errorf("cache digest = %s, want source digest %s (verbatim)", dst_desc.Digest, src_desc.Digest)
	}
	refs := listReferrers(t, cache_store, dst_ref.Context(), dst_desc.Digest)
	if len(refs) != 1 {
		t.Fatalf("cache referrers = %d, want 1", len(refs))
	}
}

// The single-manifest path preserves the digest without the verbatim index
// machinery; referrers must still travel.
func TestJobCopyReferrersSingleManifest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	up := startRegistry(t)
	cache := startRegistry(t)
	src_ref := pushImage(t, up+"/team/app:1", 2)
	src_desc, err := remote.Get(src_ref)
	if err != nil {
		t.Fatal(err)
	}
	up_store := config.StoreConfig{Name: "up", Kind: "oci", Host: up, Insecure: true}
	cache_store := config.StoreConfig{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"}
	attachReferrer(t, up_store, src_ref.Context(), src_desc)

	w, js := newCopier(t, []config.StoreConfig{up_store, cache_store}, false)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	yes := true
	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "cache", CopyReferrers: &yes})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if done.Transfers[0].Digest != src_desc.Digest.String() {
		t.Errorf("transfer digest = %q, want source digest", done.Transfers[0].Digest)
	}
	dst_ref, _ := name.ParseReference(done.Transfers[0].Ref, name.Insecure)
	if refs := listReferrers(t, cache_store, dst_ref.Context(), src_desc.Digest); len(refs) != 1 {
		t.Fatalf("cache referrers = %d, want 1", len(refs))
	}
}

func TestPlanCopyReferrersConflicts(t *testing.T) {
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
		{Name: "ro", Kind: "oci", Host: "ro.local", Insecure: true, Mode: "proxy"},
	}, true)
	w.base = context.Background()
	yes := true
	t.Run("explicit platforms conflict", func(t *testing.T) {
		_, _, err := w.Submit(Request{Ref: "a/x:1", Source: "r.io", Target: "cache", Platforms: []string{"linux/amd64"}, CopyReferrers: &yes})
		if err == nil {
			t.Error("narrowed platforms with copy_referrers must be rejected")
		}
	})
	t.Run("proxy destination", func(t *testing.T) {
		_, _, err := w.Submit(Request{Ref: "a/x:1", Source: "r.io", Target: "ro", CopyReferrers: &yes})
		if err == nil {
			t.Error("proxy destination with copy_referrers must be rejected")
		}
	})
	t.Run("no destination", func(t *testing.T) {
		_, _, err := w.Submit(Request{Ref: "a/x:1", Source: "r.io", CopyReferrers: &yes})
		if err == nil {
			t.Error("copy_referrers without `target` must be rejected, not silently ignored")
		}
	})
}

func TestCopyReferrersDefault(t *testing.T) {
	verified_hash := v1.Hash{Algorithm: "sha256", Hex: "0123456789012345678901234567890123456789012345678901234567890123"}
	plan_exec := func(t *testing.T, verifier *fakeVerifier, req Request) *execPlan {
		t.Helper()
		w, _ := newCopier(t, []config.StoreConfig{
			{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
		}, true)
		w.base = context.Background()
		if verifier != nil {
			w.SetVerifier(verifier)
		}
		p, err := w.plan(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if p.copyReferrers && p.platforms != nil {
			t.Error("effective copy_referrers must widen to all platforms")
		}
		// The job property and the hop that acts on it must not drift apart.
		if got := p.last().referrers; got != p.copyReferrers {
			t.Errorf("step referrers = %v, want the job's %v", got, p.copyReferrers)
		}
		return p
	}
	base_req := Request{Ref: "a/x:1", Source: "r.io", Target: "cache"}
	t.Run("on when verified and nothing narrowed", func(t *testing.T) {
		ex := plan_exec(t, &fakeVerifier{dg: verified_hash}, base_req)
		if !ex.copyReferrers {
			t.Error("default should be on for a verified copy-mode job")
		}
	})
	t.Run("off when the job did not verify", func(t *testing.T) {
		ex := plan_exec(t, &fakeVerifier{}, base_req) // zero hash: mode off / unsigned allowed
		if ex.copyReferrers {
			t.Error("default must not fire for an unverified job")
		}
	})
	t.Run("off when the request narrows platforms", func(t *testing.T) {
		req := base_req
		req.Platforms = []string{"linux/arm64"}
		ex := plan_exec(t, &fakeVerifier{dg: verified_hash}, req)
		if ex.copyReferrers {
			t.Error("default must respect request platform narrowing")
		}
	})
}
