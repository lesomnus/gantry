package verify

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/notaryproject/notation-core-go/testhelper"
)

// TestVerifyUnreachableIsNotUntrusted confirms that an unreachable registry is
// classified as a transient (non-sentinel) error, never as ErrUntrusted or
// ErrUnsigned — so a network blip is never cached as a verdict nor used to kill
// a container.
func TestVerifyUnreachableIsNotUntrusted(t *testing.T) {
	// A registry we tear down mid-test (startRegistry auto-closes on cleanup).
	srv := httptest.NewServer(registry.New())
	u, _ := url.Parse(srv.URL)
	host := u.Host

	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trustDir := caDir(t, root.Cert)
	ctx := context.Background()

	ref := host + "/app/signed:1"
	_, dg := pushImage(t, ref)
	signImage(t, host, ref, root, leaf)

	v, err := New(config.VerifyConfig{
		Mode: config.VerifyRequire, Provider: "notation", TrustStore: trustDir,
		Level: "permissive", Timeout: config.Duration(5 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	from := config.StoreConfig{Name: "up", Kind: "oci", Host: host, Insecure: true}
	src, err := name.NewDigest(host+"/app/signed@"+dg.String(), name.Insecure)
	if err != nil {
		t.Fatal(err)
	}

	srv.Close() // registry now unreachable

	_, err = v.Verify(ctx, from, src)
	if err == nil {
		t.Fatal("expected an error from an unreachable registry")
	}
	if errors.Is(err, ErrUntrusted) {
		t.Errorf("unreachable registry must NOT be ErrUntrusted (would cause a wrongful kill): %v", err)
	}
	if errors.Is(err, ErrUnsigned) {
		t.Errorf("unreachable registry must NOT be ErrUnsigned: %v", err)
	}
}
