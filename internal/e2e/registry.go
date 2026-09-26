package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// quietRegistry is a ggcr in-memory registry with its request logging silenced.
func quietRegistry() http.Handler {
	return registry.New(
		registry.WithReferrersSupport(true),
		registry.Logger(log.New(io.Discard, "", 0)),
	)
}

// newRegistry starts an in-memory OCI registry (referrers API enabled) on
// loopback and returns its host:port. It is a real registry served over plain
// HTTP — the source/cache backing for the hermetic tier.
func newRegistry(t *testing.T) (host string, close func()) {
	t.Helper()
	srv := httptest.NewServer(quietRegistry())
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

// newCountingRegistry is newRegistry plus a counter of completed blob uploads
// (the final PUT to /blobs/uploads/), for asserting incremental copy skips
// already-present blobs.
func newCountingRegistry(t *testing.T) (host string, uploads *int32, close func()) {
	t.Helper()
	var n int32
	inner := quietRegistry()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/uploads/") {
			atomic.AddInt32(&n, 1)
		}
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	return strings.TrimPrefix(srv.URL, "http://"), &n, srv.Close
}

// newTLSRegistry starts an in-memory OCI registry over HTTPS with a self-signed
// certificate (valid for loopback) and writes that certificate to a file usable
// as a store's ca_cert. Returns host:port and the CA file path.
func newTLSRegistry(t *testing.T) (host, caFile string, close func()) {
	t.Helper()
	srv := httptest.NewTLSServer(quietRegistry())
	caFile = filepath.Join(t.TempDir(), "ca.crt")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(srv.URL, "https://"), caFile, srv.Close
}

// newMTLSRegistry starts an in-memory OCI registry over HTTPS that REQUIRES a
// client certificate signed by its private CA, and writes the CA plus a valid
// client certificate/key pair to files usable as a store's ca_cert and kind
// "file" cred.
func newMTLSRegistry(t *testing.T) (host, caFile, certFile, keyFile string, close func()) {
	t.Helper()
	dir := t.TempDir()

	newKey := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	caKey := newKey()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "e2e-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	issue := func(serial int64, cn string, server bool) (certPEM, keyPEM []byte) {
		key := newKey()
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if server {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
			tmpl.DNSNames = []string{"localhost"}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}

	serverCertPEM, serverKeyPEM := issue(2, "server", true)
	clientCertPEM, clientKeyPEM := issue(3, "client", false)
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA")
	}

	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	caFile = write("ca.crt", caPEM)
	certFile = write("client.crt", clientCertPEM)
	keyFile = write("client.key", clientKeyPEM)

	srv := httptest.NewUnstartedServer(quietRegistry())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	srv.StartTLS()
	return strings.TrimPrefix(srv.URL, "https://"), caFile, certFile, keyFile, srv.Close
}

func insecureTag(t *testing.T, host, repo, tag string) name.Tag {
	t.Helper()
	ref, err := name.NewTag(fmt.Sprintf("%s/%s:%s", host, repo, tag), name.Insecure)
	if err != nil {
		t.Fatalf("parse tag: %v", err)
	}
	return ref
}

// seedImage pushes a random single-platform image to host/repo:tag and returns
// its manifest digest.
func seedImage(t *testing.T, host, repo, tag string) v1.Hash {
	t.Helper()
	img, err := random.Image(2048, 3)
	if err != nil {
		t.Fatalf("random image: %v", err)
	}
	if err := remote.Write(insecureTag(t, host, repo, tag), img); err != nil {
		t.Fatalf("push seed image: %v", err)
	}
	h, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedIndex pushes a random multi-platform index and returns its digest.
func seedIndex(t *testing.T, host, repo, tag string) v1.Hash {
	t.Helper()
	idx, err := random.Index(2048, 2, 2)
	if err != nil {
		t.Fatalf("random index: %v", err)
	}
	if err := remote.WriteIndex(insecureTag(t, host, repo, tag), idx); err != nil {
		t.Fatalf("push seed index: %v", err)
	}
	h, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedPlatformIndex pushes an index with one child image per given platform
// ("os/arch") and returns its digest, so platform selection can be exercised.
func seedPlatformIndex(t *testing.T, host, repo, tag string, platforms ...string) v1.Hash {
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
	if err := remote.WriteIndex(insecureTag(t, host, repo, tag), idx); err != nil {
		t.Fatalf("push platform index: %v", err)
	}
	h, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// hasTag reports whether host holds repo:tag.
func hasTag(t *testing.T, host, repo, tag string) bool {
	t.Helper()
	_, err := remote.Head(insecureTag(t, host, repo, tag))
	return err == nil
}

// digestByRef resolves host/repo@digest, proving a manifest is present by digest.
func digestByRef(t *testing.T, host, repo, digest string) (v1.Hash, error) {
	t.Helper()
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", host, repo, digest), name.Insecure)
	if err != nil {
		return v1.Hash{}, err
	}
	d, err := remote.Head(ref)
	if err != nil {
		return v1.Hash{}, err
	}
	return d.Digest, nil
}

// digestOf returns the manifest digest of host/repo:tag.
func digestOf(t *testing.T, host, repo, tag string) (v1.Hash, error) {
	t.Helper()
	d, err := remote.Head(insecureTag(t, host, repo, tag))
	if err != nil {
		return v1.Hash{}, err
	}
	return d.Digest, nil
}
