package cpx

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// childOf returns the descriptor the index names for a platform.
func childOf(t *testing.T, ref name.Reference, platform string) v1.Descriptor {
	t.Helper()
	desc, err := remote.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range im.Manifests {
		if m.Platform != nil && m.Platform.String() == platform {
			return m
		}
	}
	t.Fatalf("index %q has no %s child", ref.Name(), platform)
	return v1.Descriptor{}
}

func parseRef(t *testing.T, s string) name.Reference {
	t.Helper()
	r, err := name.ParseReference(s, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A copy narrowed to ONE platform commits that child manifest itself, so the
// cache holds a digest the source published rather than one gantry authored.
// That digest is what a signature is over, what a referrer's subject names, and
// what an engine records when it pulls — a rebuilt one-entry index is none of
// those things, which is the whole reason for the shape.
func TestCommitNarrowedToOnePlatformCommitsTheChild(t *testing.T) {
	ctx := context.Background()
	up := startRegistry(t)
	host := startRegistry(t)
	src := pushIndex(t, up+"/src/multi:1", "linux/amd64", "linux/arm64", "linux/s390x")
	dst := parseRef(t, host+"/cache/multi:1")
	want := childOf(t, src, "linux/amd64")

	s, err := NewSource(reg("source", up), reg("target", host))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.Resolve(ctx, src, dst, []string{"linux/amd64"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.Layers) != 3 { // 2 layers + 1 config, amd64 only
		t.Fatalf("plan layers = %d, want 3 (one platform)", len(plan.Layers))
	}
	for _, l := range plan.Layers {
		if err := s.Fill(ctx, src.Context(), dst.Context(), l, &testSink{}); err != nil {
			t.Fatalf("fill %s: %v", l.Digest, err)
		}
	}

	committed, err := s.Commit(ctx, src, dst, []string{"linux/amd64"}, false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if committed != want.Digest {
		t.Errorf("committed %s, want the source's child digest %s", committed, want.Digest)
	}

	got, err := remote.Get(dst)
	if err != nil {
		t.Fatalf("cache tag not resolvable: %v", err)
	}
	if got.MediaType.IsIndex() {
		t.Errorf("cache holds %s; a narrowed copy must not wrap the child in an index", got.MediaType)
	}
	if got.Digest != want.Digest {
		t.Errorf("cache tag resolves to %s, want %s", got.Digest, want.Digest)
	}
	// Byte-identity, not just digest equality: the digest is computed from these
	// bytes, so a mismatch here would mean the comparison above proved nothing.
	upstream, err := remote.Get(parseRef(t, up+"/src/multi@"+want.Digest.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Manifest, upstream.Manifest) {
		t.Errorf("cached manifest differs from the source's child manifest")
	}
}

// Narrowing to several platforms still needs an index to name them, so the
// flatten is strictly the single-child case.
func TestCommitNarrowedToSeveralPlatformsBuildsAnIndex(t *testing.T) {
	ctx := context.Background()
	host := startRegistry(t)
	src := pushIndex(t, host+"/src/multi:1", "linux/amd64", "linux/arm64", "linux/s390x")
	dst := parseRef(t, host+"/cache/multi:1")

	s, err := NewSource(reg("source", host), reg("target", host))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, src, dst, []string{"linux/amd64", "linux/arm64"}, false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := remote.Get(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !got.MediaType.IsIndex() {
		t.Fatalf("cache holds %s, want an index", got.MediaType)
	}
	idx, err := got.ImageIndex()
	if err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Manifests) != 2 {
		t.Errorf("index has %d children, want 2", len(im.Manifests))
	}
}

// An index that happens to hold one child is NOT a narrowing — buildx publishes
// single-platform images that way — so an unnarrowed copy of one must stay an
// index. Flattening here would change the digest of a copy whose caller asked
// to take the image whole.
func TestCommitUnnarrowedIndexWithOneChildStaysAnIndex(t *testing.T) {
	ctx := context.Background()
	host := startRegistry(t)
	src := pushIndex(t, host+"/src/single:1", "linux/amd64")
	dst := parseRef(t, host+"/cache/single:1")

	s, err := NewSource(reg("source", host), reg("target", host))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, src, dst, nil, false); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := remote.Get(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaType.IsIndex() {
		return
	}
	t.Errorf("cache holds %s; an unnarrowed copy must stay an index", got.MediaType)
}

// A platform the index does not carry must fail the commit rather than publish
// the empty index the filter would otherwise build.
func TestCommitNoPlatformMatchFails(t *testing.T) {
	ctx := context.Background()
	host := startRegistry(t)
	src := pushIndex(t, host+"/src/multi:1", "linux/amd64", "linux/arm64")
	dst := parseRef(t, host+"/cache/multi:1")

	s, err := NewSource(reg("source", host), reg("target", host))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Commit(ctx, src, dst, []string{"linux/ppc64le"}, false)
	if err == nil {
		t.Fatal("commit succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "no manifest matched") {
		t.Errorf("error = %v, want it to name the unmatched platforms", err)
	}
	if _, err := remote.Get(dst); err == nil {
		t.Error("cache tag was published despite the failed commit")
	}
}

// The point of committing the child: a signature the source attached to that
// platform's manifest travels to the cache with a subject that EXISTS there, so
// the cache copy verifies as the source's own signed artifact. Against a
// one-entry index the subject would be a digest no signature is over.
func TestNarrowedCommitCarriesTheChildSignature(t *testing.T) {
	ctx := context.Background()
	up := startRegistry(t)
	host := startRegistry(t)
	src := pushIndex(t, up+"/src/multi:1", "linux/amd64", "linux/arm64")
	dst := parseRef(t, host+"/cache/multi:1")
	child := childOf(t, src, "linux/amd64")

	// Sign the platform manifest upstream, the way a per-platform `notation sign`
	// leaves it.
	childRef := parseRef(t, up+"/src/multi@"+child.Digest.String())
	childDesc, err := remote.Get(childRef)
	if err != nil {
		t.Fatal(err)
	}
	source := reg("source", up)
	target := reg("target", host)
	attachReferrer(t, source, childRef.Context(), childDesc)

	s, err := NewSource(source, target)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := s.Commit(ctx, src, dst, []string{"linux/amd64"}, false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	// What move.go does after a commit: the subject is the digest the commit
	// returned, so the flatten is what makes this reach the child's signature.
	n, err := copyReferrers(ctx, source, target, src, committed, dst.Context())
	if err != nil {
		t.Fatalf("copy referrers: %v", err)
	}
	if n != 1 {
		t.Fatalf("copied %d referrers, want 1", n)
	}
	if got := listReferrers(t, target, dst.Context(), committed); len(got) != 1 {
		t.Errorf("cache serves %d referrers over %s, want 1", len(got), committed)
	}
}
