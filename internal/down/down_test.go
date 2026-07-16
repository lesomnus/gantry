package down

import (
	"context"
	"sync"
	"testing"

	"github.com/lesomnus/gantry/cmd/config"
)

type nopSink struct{}

func (nopSink) Layer(LayerUpdate) {}

// recSink records the latest update per layer digest (concurrency-safe).
type recSink struct {
	mu     sync.Mutex
	layers map[string]LayerUpdate
}

// Layer mirrors the warm engineSink: it preserves a layer's total and, on
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
