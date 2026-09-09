package down

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
)

type nopSink struct{}

func (nopSink) Layer(LayerUpdate) {}

// recSink records the latest update per layer digest (concurrency-safe).
type recSink struct {
	mu     sync.Mutex
	layers map[string]LayerUpdate
}

// Layer mirrors the cpx engineSink: it preserves a layer's total and, on
// done/exists, sets done to that total.
func (s *recSink) Layer(u LayerUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.layers == nil {
		s.layers = map[string]LayerUpdate{}
	}
	cur := s.layers[u.Digest]
	cur.Digest = u.Digest
	if u.Total > 0 {
		cur.Total = u.Total
	}
	switch u.State {
	case "exists":
		cur.Done = cur.Total
	case "done":
		if cur.Total > 0 {
			cur.Done = cur.Total
		}
	default:
		if u.Done > cur.Done {
			cur.Done = u.Done
		}
	}
	cur.State = u.State
	s.layers[u.Digest] = cur
}

func (s *recSink) bytesDone() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, u := range s.layers {
		n += u.Done
	}
	return n
}

// progressed reports whether any per-layer progress was observed — bytes moved,
// or (when the daemon's containerd image store reports state-only on fast local
// pulls, docker 29+) at least a layer state. Engine byte counts are best-effort.
func (s *recSink) progressed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.layers {
		if u.Done > 0 || u.State != "" {
			return true
		}
	}
	return false
}

type fakeEngine struct{ name string }

func (f *fakeEngine) Name() string                { return f.name }
func (f *fakeEngine) Kind() string                { return "fake" }
func (f *fakeEngine) Ready(context.Context) error { return nil }
func (f *fakeEngine) Pull(context.Context, string, string, string, []string, *AnchorBlob, Sink) ([]string, error) {
	return nil, nil
}
func (f *fakeEngine) Platform(context.Context) (string, error)       { return "linux/amd64", nil }
func (f *fakeEngine) InUse(context.Context) (map[string]bool, error) { return nil, nil }
func (f *fakeEngine) SeedUsage(context.Context, UsageSink) error     { return nil }
func (f *fakeEngine) WatchUsage(context.Context, UsageSink) error    { return nil }
func (f *fakeEngine) Remove(context.Context, string) (RemoveResult, error) {
	return RemoveResult{}, nil
}
func (f *fakeEngine) Close() error { return nil }

type verifyingEngine struct{ fakeEngine }

func (verifyingEngine) Verify(context.Context, string) error { return nil }

func TestDockerHost(t *testing.T) {
	cases := map[string]string{
		"/var/run/docker.sock": "unix:///var/run/docker.sock",
		"tcp://docker:2375":    "tcp://docker:2375",
		"unix:///x.sock":       "unix:///x.sock",
	}
	for in, want := range cases {
		if got := dockerHost(in); got != want {
			t.Errorf("dockerHost(%q) = %q, want %q", in, got, want)
		}
	}
	if dockerHost("") == "" {
		t.Error("empty address should fall back to a default host")
	}
}

func TestCapabilities(t *testing.T) {
	if c := Capabilities(&fakeEngine{}); !c.Pull || c.Verify || c.GC {
		t.Errorf("plain engine caps = %+v", c)
	}
	if c := Capabilities(&verifyingEngine{}); !c.Verify || c.GC {
		t.Errorf("verifying engine caps = %+v", c)
	}
}

func TestNewRejectsNonEngine(t *testing.T) {
	if _, err := New(config.StoreConfig{Name: "x", Kind: "oci"}); err == nil {
		t.Error("registry is not an engine kind")
	}
	if _, err := New(config.StoreConfig{Name: "x", Kind: "bogus"}); err == nil {
		t.Error("unknown kind should error")
	}
}

func TestNewDockerEngineTLSWiring(t *testing.T) {
	// No TLS fields: builds a plain client (transport is nil, daemon dialed as-is).
	if _, err := newDockerEngine(config.StoreConfig{Name: "plain", Kind: "docker", Address: "tcp://127.0.0.1:2375"}); err != nil {
		t.Errorf("plain docker engine should build: %v", err)
	}
	// insecure: builds a TLS-skip transport (no files needed) and dials https.
	if _, err := newDockerEngine(config.StoreConfig{Name: "ins", Kind: "docker", Address: "tcp://127.0.0.1:2376", Insecure: true}); err != nil {
		t.Errorf("insecure docker engine should build: %v", err)
	}
	// A bad ca_cert path must surface as an engine build error — proving the
	// store transport is wired into the docker client.
	_, err := newDockerEngine(config.StoreConfig{Name: "ca", Kind: "docker", Address: "tcp://127.0.0.1:2376", CACert: "/no/such/ca.crt"})
	if err == nil {
		t.Error("a missing ca_cert should fail docker engine construction")
	}
	// A cred: the store transport may be a WRAPPER round tripper rather than the
	// bare *http.Transport, and the docker client configures the concrete type
	// (client.WithHost -> sockets.ConfigureTransport). An engine that hands it
	// the wrapper fails to build at all — "cannot apply host to transport" — so
	// every mTLS docker store is dead at startup. That is not visible from
	// xport's own tests, which never build a docker client.
	cert, key := writeKeyPair(t)
	if _, err := newDockerEngine(config.StoreConfig{
		Name: "mtls", Kind: "docker", Address: "tcp://127.0.0.1:2376",
		Cred: &config.CredConfig{Kind: "file", Cert: cert, Key: key},
	}); err != nil {
		t.Errorf("an mTLS docker engine should build: %v", err)
	}
}

// A docker daemon on a TLS port has to be spoken to over TLS. The store's
// transport may be a wrapper (xport annotates certificate alerts with what
// gantry presented), and the docker client reads the concrete *http.Transport
// to decide the scheme — so a wrapper it cannot see through makes it dial
// https-over-http and the daemon answers "client sent an HTTP request to an
// HTTPS server". Nothing about that is visible until a real TLS daemon is on
// the other end, which is what this test is.
func TestDockerEngineSpeaksTLSToATLSDaemon(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.51")
		w.Header().Set("Ostype", "linux")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The server signs for itself, so its certificate is also its CA.
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, key := writeKeyPair(t)

	eng, err := newDockerEngine(config.StoreConfig{
		Name: "tls", Kind: "docker",
		Address: "tcp://" + srv.Listener.Addr().String(),
		CACert:  caPath,
		Cred:    &config.CredConfig{Kind: "file", Cert: cert, Key: key},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := eng.Ready(context.Background()); err != nil {
		t.Fatalf("ping over TLS: %v", err)
	}
}

// writeKeyPair writes a self-signed certificate and its key, and returns the
// two paths.
func writeKeyPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "docker client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "client.crt")
	keyPath = filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestOCIPlatformNormalization(t *testing.T) {
	cases := map[[2]string]string{
		{"linux", "x86_64"}:  "linux/amd64",
		{"linux", "aarch64"}: "linux/arm64",
		{"linux", "amd64"}:   "linux/amd64",
		{"linux", "armhf"}:   "linux/arm/v7",
	}
	for in, want := range cases {
		got, err := ociPlatform(in[0], in[1])
		if err != nil {
			t.Errorf("ociPlatform(%q, %q): %v", in[0], in[1], err)
			continue
		}
		if got != want {
			t.Errorf("ociPlatform(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestDigestOf(t *testing.T) {
	if got := digestOf("layer-sha256:abc"); got != "sha256:abc" {
		t.Errorf("digestOf = %q", got)
	}
	if got := digestOf("no-digest-here"); got != "" {
		t.Errorf("digestOf = %q, want empty", got)
	}
}
