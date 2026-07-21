package xport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
)

// The TPM signer is a crypto.Signer whose private key lives in the device. These
// tests substitute an in-memory *ecdsa.PrivateKey (also a crypto.Signer) so the
// transport-building path — cert/key matching, chain assembly, mTLS handshake —
// is exercised end to end without a real TPM.

func genCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func issueCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

func encodeKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// TestMTLSTransportFullHandshake proves the transport built by mtlsTransport
// presents its client certificate (signed on the fly by the supplied
// crypto.Signer) to a server that requires and verifies it, while also verifying
// the server against the configured CA. This is the exact wiring the TPM signer
// gets — the only difference in production is where Sign runs.
func TestMTLSTransportFullHandshake(t *testing.T) {
	ca, caKey, caPEM := genCA(t)
	serverCertPEM, serverKey := issueCert(t, ca, caKey, "server", true)
	clientCertPEM, clientKey := issueCert(t, ca, caKey, "client", false)

	serverCert, err := tls.X509KeyPair(serverCertPEM, encodeKey(t, serverKey))
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no client cert", http.StatusBadRequest)
			return
		}
		io.WriteString(w, r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	// clientKey stands in for the TPM signer (both are crypto.Signer).
	rt, err := mtlsTransport("the key at the TPM handle", clientCertPEM, caPEM, clientKey, false)
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	resp, err := (&http.Client{Transport: rt}).Get(srv.URL)
	if err != nil {
		t.Fatalf("mTLS GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "client" {
		t.Fatalf("status %d body %q; want 200 \"client\"", resp.StatusCode, body)
	}
}

func TestMTLSTransportKeyMismatch(t *testing.T) {
	ca, caKey, _ := genCA(t)
	certPEM, _ := issueCert(t, ca, caKey, "client", false)
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mtlsTransport("the key at the TPM handle", certPEM, nil, otherKey, false); err == nil {
		t.Error("expected error when signer does not match the certificate key")
	}
}

func TestMTLSTransportRejectsBadCert(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mtlsTransport("the key at the TPM handle", []byte("not a pem"), nil, key, false); err == nil {
		t.Error("expected error for a certificate file with no CERTIFICATE block")
	}
}

func TestMTLSTransportRejectsBadCA(t *testing.T) {
	ca, caKey, _ := genCA(t)
	certPEM, key := issueCert(t, ca, caKey, "client", false)
	if _, err := mtlsTransport("the key at the TPM handle", certPEM, []byte("garbage ca"), key, false); err == nil {
		t.Error("expected error for an unparseable ca_cert")
	}
}

// TestKeyPairTransportFullHandshake proves a store configured with a kind
// "file" cred (a PEM cert/key file pair, no TPM) presents its certificate to a
// server that requires and verifies it, through the public Transport entry
// point — including the memoization the TPM path gets.
func TestKeyPairTransportFullHandshake(t *testing.T) {
	ca, caKey, caPEM := genCA(t)
	serverCertPEM, serverKey := issueCert(t, ca, caKey, "server", true)
	clientCertPEM, clientKey := issueCert(t, ca, caKey, "client", false)

	serverCert, err := tls.X509KeyPair(serverCertPEM, encodeKey(t, serverKey))
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("append CA")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no client cert", http.StatusBadRequest)
			return
		}
		io.WriteString(w, r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c := config.StoreConfig{
		Cred: &config.CredConfig{
			Kind: "file",
			Cert: write("client.crt", clientCertPEM),
			Key:  write("client.key", encodeKey(t, clientKey)),
		},
		CACert: write("ca.crt", caPEM),
	}
	defer CloseTPM() // clear the cache for other tests

	rt, err := Transport(c)
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	resp, err := (&http.Client{Transport: rt}).Get(srv.URL)
	if err != nil {
		t.Fatalf("mTLS GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "client" {
		t.Fatalf("status %d body %q; want 200 \"client\"", resp.StatusCode, body)
	}

	again, err := Transport(c)
	if err != nil {
		t.Fatal(err)
	}
	if again != rt {
		t.Error("Transport should return the same cached transport for identical config")
	}
}

// A cred.cert issued for a different key than cred.key must fail the
// transport build with a config error, not an opaque handshake failure.
func TestKeyPairTransportKeyMismatch(t *testing.T) {
	ca, caKey, _ := genCA(t)
	certPEM, _ := issueCert(t, ca, caKey, "client", false)
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, encodeKey(t, otherKey), 0o600); err != nil {
		t.Fatal(err)
	}
	defer CloseTPM()

	c := config.StoreConfig{Cred: &config.CredConfig{Kind: "file", Cert: certPath, Key: keyPath}}
	if _, err := Transport(c); err == nil {
		t.Error("expected error when cred.key does not match cred.cert")
	}
}

func TestParsePrivateKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("PKCS#8", func(t *testing.T) {
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
		s, err := parsePrivateKey(pemBytes)
		if err != nil {
			t.Fatal(err)
		}
		if !key.PublicKey.Equal(s.Public()) {
			t.Error("parsed key does not match")
		}
	})
	t.Run("SEC1 EC", func(t *testing.T) {
		s, err := parsePrivateKey(encodeKey(t, key))
		if err != nil {
			t.Fatal(err)
		}
		if !key.PublicKey.Equal(s.Public()) {
			t.Error("parsed key does not match")
		}
	})
	t.Run("key block after a certificate block", func(t *testing.T) {
		ca, caKey, _ := genCA(t)
		certPEM, _ := issueCert(t, ca, caKey, "client", false)
		combined := append(append([]byte{}, certPEM...), encodeKey(t, key)...)
		if _, err := parsePrivateKey(combined); err != nil {
			t.Errorf("combined cert+key file should parse: %v", err)
		}
	})
	t.Run("no key block", func(t *testing.T) {
		if _, err := parsePrivateKey([]byte("not a pem")); err == nil {
			t.Error("expected error for a file with no private-key block")
		}
	})
	t.Run("corrupt key block", func(t *testing.T) {
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})
		if _, err := parsePrivateKey(pemBytes); err == nil {
			t.Error("expected error for an unparseable key block")
		}
	})
}

func TestTransportFallback(t *testing.T) {
	// No TPM, no ca_cert, not insecure: nil, so the library default is used.
	rt, err := Transport(config.StoreConfig{})
	if err != nil || rt != nil {
		t.Fatalf("plain store: got %v, %v; want nil, nil", rt, err)
	}

	// Insecure (no TPM): a TLS-verify-skip transport, preserving prior behavior.
	rt, err = Transport(config.StoreConfig{Insecure: true})
	if err != nil || rt == nil {
		t.Fatalf("insecure store: got %v, %v; want transport, nil", rt, err)
	}
	tr, ok := rt.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("insecure store transport should skip TLS verification")
	}
}

// A store may set ca_cert alone to verify a private-CA registry over ordinary
// TLS, with no TPM client certificate. The transport must trust the CA, still
// verify the server, and present no client cert.
func TestTransportCACertOnly(t *testing.T) {
	_, _, caPEM := genCA(t)
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	rt, err := Transport(config.StoreConfig{CACert: caPath})
	if err != nil || rt == nil {
		t.Fatalf("ca-only store: got %v, %v; want transport, nil", rt, err)
	}
	tr := rt.(*http.Transport)
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("ca-only transport should set RootCAs")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("ca-only transport must still verify the server")
	}
	if len(tr.TLSClientConfig.Certificates) != 0 {
		t.Error("ca-only transport must not present a client certificate")
	}
}

// Transport memoizes successful builds: the same config returns the identical
// *http.Transport so the connection pool (and, for TPM, the open device) is
// reused across jobs, verification, and health probes.
func TestTransportMemoizesSuccess(t *testing.T) {
	_, _, caPEM := genCA(t)
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	defer CloseTPM() // clear the cache for other tests

	c := config.StoreConfig{CACert: caPath}
	a, err := Transport(c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Transport(c)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("Transport should return the same cached transport for identical config")
	}
}

// A transient build failure must NOT be cached: once the underlying condition
// clears (here, the ca_cert file appears), the next call must succeed.
func TestTransportDoesNotCacheErrors(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	c := config.StoreConfig{CACert: caPath}
	defer CloseTPM()

	if _, err := Transport(c); err == nil {
		t.Fatal("expected error while ca_cert file is missing")
	}

	_, _, caPEM := genCA(t)
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := Transport(c)
	if err != nil || rt == nil {
		t.Fatalf("after the file appears: got %v, %v; want transport, nil", rt, err)
	}
}
