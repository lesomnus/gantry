package verify

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/notaryproject/notation-core-go/signature/jws"
	"github.com/notaryproject/notation-core-go/testhelper"
	"github.com/notaryproject/notation-go"
	notationregistry "github.com/notaryproject/notation-go/registry"
	"github.com/notaryproject/notation-go/signer"
	ocistore "oras.land/oras-go/v2/content/oci"
)

// signImageInLayout writes a random image to an on-disk OCI layout at dir and
// signs it there (the signature is stored as a referrer in the same layout). The
// ref host is unroutable so a test can prove verification never touched a
// registry. Returns the ref and the image digest.
func signImageInLayout(t *testing.T, dir string, root, leaf testhelper.RSACertTuple) (string, v1.Hash) {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.AppendImage(img); err != nil {
		t.Fatal(err)
	}
	s, err := signer.New(leaf.PrivateKey, []*x509.Certificate{leaf.Cert, root.Cert})
	if err != nil {
		t.Fatal(err)
	}
	st, err := ocistore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1:1 is unroutable: if verification ever falls through to the
	// registry for this ref, the dial fails loudly.
	ref := "127.0.0.1:1/app@" + dg.String()
	if _, err := notation.Sign(context.Background(), s, notationregistry.NewRepository(st), notation.SignOptions{
		SignerSignOptions: notation.SignerSignOptions{SignatureMediaType: jws.MediaTypeEnvelope},
		ArtifactReference: ref,
	}); err != nil {
		t.Fatalf("sign into layout: %v", err)
	}
	return ref, dg
}

func layoutVerifier(t *testing.T, trust, localLayout string) Verifier {
	t.Helper()
	v, err := New(config.VerifyConfig{
		Mode: config.VerifyRequire, Provider: "notation", TrustStore: trust,
		Level: "permissive", Timeout: config.Duration(20 * time.Second), LocalLayout: localLayout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyLocalLayoutOffline(t *testing.T) {
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trustDir := caDir(t, root.Cert)
	ctx := context.Background()

	dir := t.TempDir()
	ref, want := signImageInLayout(t, dir, root, leaf)
	src, err := name.NewDigest(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	// The source store points at the (unroutable) ref host; a passing verify
	// proves the signature came from the local layout with no network access.
	from := config.StoreConfig{Name: "up", Kind: "oci", Host: "127.0.0.1:1", Insecure: true}

	got, err := layoutVerifier(t, trustDir, dir).Verify(ctx, from, src)
	if err != nil {
		t.Fatalf("offline verify via local layout: %v", err)
	}
	if got.Digest.String() != want.String() {
		t.Errorf("verified digest = %s, want %s", got.Digest, want)
	}
}

func TestVerifyLocalLayoutFallThroughToRegistry(t *testing.T) {
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trustDir := caDir(t, root.Cert)
	ctx := context.Background()

	// The layout holds some *other* signed image; the requested image lives only
	// in the registry. Verification must fall through and succeed.
	dir := t.TempDir()
	signImageInLayout(t, dir, root, leaf)

	host := startRegistry(t)
	ref := host + "/app/only-remote:1"
	_, want := pushImage(t, ref)
	signImage(t, host, ref, root, leaf)
	src, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	from := config.StoreConfig{Name: "up", Kind: "oci", Host: host, Insecure: true}

	got, err := layoutVerifier(t, trustDir, dir).Verify(ctx, from, src)
	if err != nil {
		t.Fatalf("fall-through verify: %v", err)
	}
	if got.Digest.String() != want.String() {
		t.Errorf("verified digest = %s, want %s", got.Digest, want)
	}
}

func TestVerifyLocalLayoutBadPathFailsFast(t *testing.T) {
	root := testhelper.GetRSARootCertificate()
	trustDir := caDir(t, root.Cert)
	_, err := New(config.VerifyConfig{
		Mode: config.VerifyRequire, Provider: "notation", TrustStore: trustDir,
		Level: "permissive", LocalLayout: t.TempDir() + "/does-not-exist",
	})
	if err == nil {
		t.Fatal("expected fail-fast on a non-existent local_layout")
	}
	if !errors.Is(err, ErrBadTrustMaterial) {
		t.Errorf("err = %v, want ErrBadTrustMaterial", err)
	}
}
