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

type fakeEngine struct{ name string }

func (f *fakeEngine) Name() string                                   { return f.name }
func (f *fakeEngine) Kind() string                                   { return "fake" }
func (f *fakeEngine) Ready(context.Context) error                    { return nil }
func (f *fakeEngine) Pull(context.Context, string, Sink) error       { return nil }
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

func TestDigestOf(t *testing.T) {
	if got := digestOf("layer-sha256:abc"); got != "sha256:abc" {
		t.Errorf("digestOf = %q", got)
	}
	if got := digestOf("no-digest-here"); got != "" {
		t.Errorf("digestOf = %q, want empty", got)
	}
}
