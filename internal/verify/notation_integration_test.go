package verify

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/notaryproject/notation-core-go/signature/jws"
	"github.com/notaryproject/notation-core-go/testhelper"
	"github.com/notaryproject/notation-go"
	notationregistry "github.com/notaryproject/notation-go/registry"
	"github.com/notaryproject/notation-go/signer"
	orasremote "oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

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

func pushImage(t *testing.T, ref string) (name.Reference, v1.Hash) {
	t.Helper()
	img, err := random.Image(256, 2)
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
	dg, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return r, dg
}

// signImage signs ref in-process with a fresh leaf chaining to root, pushing the
// notation signature as a referrer to the (in-memory) registry.
func signImage(t *testing.T, host, ref string, root, leaf testhelper.RSACertTuple) {
	t.Helper()
	s, err := signer.New(leaf.PrivateKey, []*x509.Certificate{leaf.Cert, root.Cert})
	if err != nil {
		t.Fatal(err)
	}
	r, err := orasremote.NewRepository(mustRepo(t, ref))
	if err != nil {
		t.Fatal(err)
	}
	r.PlainHTTP = true
	r.Client = &auth.Client{Cache: auth.NewCache()}
	if _, err := notation.Sign(context.Background(), s, notationregistry.NewRepository(r), notation.SignOptions{
		SignerSignOptions: notation.SignerSignOptions{SignatureMediaType: jws.MediaTypeEnvelope},
		ArtifactReference: ref,
	}); err != nil {
		t.Fatalf("notation sign: %v", err)
	}
}

func mustRepo(t *testing.T, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return r.Context().Name()
}

// caDir writes a CA cert to a fresh dir and returns the dir (a trust store).
func caDir(t *testing.T, ca *x509.Certificate) string {
	t.Helper()
	dir := t.TempDir()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	if err := os.WriteFile(filepath.Join(dir, "root.crt"), pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerifyNotationLive(t *testing.T) {
	host := startRegistry(t)
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trustDir := caDir(t, root.Cert)
	from := config.StoreConfig{Name: "up", Kind: "oci", Host: host, Insecure: true}
	ctx := context.Background()

	newVerifier := func(t *testing.T, mode config.VerifyMode, trust string) Verifier {
		v, err := New(config.VerifyConfig{
			Mode: mode, Provider: "notation", TrustStore: trust,
			Level: "permissive", Timeout: config.Duration(20 * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	srcOf := func(t *testing.T, ref string) name.Reference {
		r, err := name.ParseReference(ref, name.Insecure)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("signed image verifies and returns the digest", func(t *testing.T) {
		ref := host + "/app/signed:1"
		_, want := pushImage(t, ref)
		signImage(t, host, ref, root, leaf)

		got, err := newVerifier(t, config.VerifyRequire, trustDir).Verify(ctx, from, srcOf(t, ref))
		if err != nil {
			t.Fatalf("verify signed image: %v", err)
		}
		if got.String() != want.String() {
			t.Errorf("verified digest = %s, want %s", got, want)
		}
	})

	t.Run("require rejects an unsigned image", func(t *testing.T) {
		ref := host + "/app/unsigned:1"
		pushImage(t, ref)
		_, err := newVerifier(t, config.VerifyRequire, trustDir).Verify(ctx, from, srcOf(t, ref))
		if !errors.Is(err, ErrUnsigned) {
			t.Errorf("err = %v, want ErrUnsigned", err)
		}
	})

	t.Run("verify-if-present allows an unsigned image", func(t *testing.T) {
		ref := host + "/app/unsigned2:1"
		pushImage(t, ref)
		got, err := newVerifier(t, config.VerifyIfPresent, trustDir).Verify(ctx, from, srcOf(t, ref))
		if err != nil {
			t.Fatalf("verify-if-present on unsigned should pass: %v", err)
		}
		if got.Hex != "" {
			t.Errorf("unsigned image should not pin a digest, got %s", got)
		}
	})

	t.Run("wrong trust anchor is untrusted", func(t *testing.T) {
		ref := host + "/app/signed2:1"
		pushImage(t, ref)
		signImage(t, host, ref, root, leaf)

		wrongCA := t.TempDir()
		writeTestCert(t, wrongCA, "other.crt") // a different, unrelated CA
		_, err := newVerifier(t, config.VerifyRequire, wrongCA).Verify(ctx, from, srcOf(t, ref))
		if !errors.Is(err, ErrUntrusted) {
			t.Errorf("err = %v, want ErrUntrusted", err)
		}
	})
}
