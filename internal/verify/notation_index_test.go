package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/notaryproject/notation-core-go/testhelper"
)

// pushPlatformIndex pushes a multi-arch index (one child image per platform) and
// returns the index digest and one child (platform-specific) manifest digest.
func pushPlatformIndex(t *testing.T, host, repo, tag string, platforms ...string) (idxDigest, childDigest v1.Hash) {
	t.Helper()
	idx := v1.ImageIndex(empty.Index)
	var adds []mutate.IndexAddendum
	for _, p := range platforms {
		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatal(err)
		}
		os, arch, _ := strings.Cut(p, "/")
		adds = append(adds, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: os, Architecture: arch}},
		})
	}
	idx = mutate.AppendManifests(idx, adds...)
	ref, err := name.ParseReference(host+"/"+repo+":"+tag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatalf("push index: %v", err)
	}
	if idxDigest, err = idx.Digest(); err != nil {
		t.Fatal(err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	return idxDigest, im.Manifests[0].Digest
}

// TestVerifyIndexSignatureSemantics documents how a signed multi-arch image is
// verified: the signature is over the TOP-LEVEL INDEX, so verifying the index
// digest is trusted, while verifying an individual platform (child) manifest
// digest finds no signature. This is why enforcement/verification keys on the
// index digest (a container's RepoDigest) rather than the platform manifest.
func TestVerifyIndexSignatureSemantics(t *testing.T) {
	host := startRegistry(t)
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trustDir := caDir(t, root.Cert)
	ctx := context.Background()

	idxDigest, childDigest := pushPlatformIndex(t, host, "app/multi", "1", "linux/amd64", "linux/arm64")
	// signImage signs the ref, which resolves to the index — so the signature is
	// a referrer of the INDEX digest.
	signImage(t, host, host+"/app/multi:1", root, leaf)

	v, err := New(config.VerifyConfig{
		Mode: config.VerifyRequire, Provider: "notation", TrustStore: trustDir,
		Level: "permissive", Timeout: config.Duration(20 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	from := config.StoreConfig{Name: "up", Kind: "oci", Host: host, Insecure: true}

	// Verifying the index (by tag) is trusted and pins the INDEX digest.
	tag, err := name.ParseReference(host+"/app/multi:1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Verify(ctx, from, tag)
	if err != nil {
		t.Fatalf("verify signed index: %v", err)
	}
	if got.Digest.String() != idxDigest.String() {
		t.Errorf("pinned digest = %s, want the index digest %s", got.Digest, idxDigest)
	}

	// Verifying an individual PLATFORM (child) manifest digest is unsigned: the
	// signature lives on the index, not the child. Enforcement never keys on this
	// digest (it uses the container's RepoDigest = the index digest).
	child, err := name.NewDigest(host+"/app/multi@"+childDigest.String(), name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, from, child); !errors.Is(err, ErrUnsigned) {
		t.Errorf("child platform manifest should be unsigned, got %v", err)
	}
}
