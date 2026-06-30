package down

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/warm"
)

type fakeTarget struct {
	name   string
	pull   func() error
	pulled string // last ref it was told to pull
}

func (f *fakeTarget) Name() string                { return f.name }
func (f *fakeTarget) Kind() string                { return "fake" }
func (f *fakeTarget) Ready(context.Context) error { return nil }
func (f *fakeTarget) Pull(_ context.Context, ref string) error {
	f.pulled = ref
	if f.pull == nil {
		return nil
	}
	return f.pull()
}
func (f *fakeTarget) Close() error { return nil }

type verifyingTarget struct{ fakeTarget }

func (verifyingTarget) Verify(context.Context, string) error { return nil }

func registryOf(targets ...Target) *Registry {
	r := &Registry{byName: map[string]Target{}}
	for _, t := range targets {
		r.entries = append(r.entries, entry{cfg: config.TargetConfig{Name: t.Name(), Kind: t.Kind()}, target: t})
		r.byName[t.Name()] = t
	}
	return r
}

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
	if c := Capabilities(&fakeTarget{}); c.Pull != true || c.Verify || c.GC {
		t.Errorf("plain target caps = %+v", c)
	}
	if c := Capabilities(&verifyingTarget{}); !c.Verify || c.GC {
		t.Errorf("verifying target caps = %+v", c)
	}
}

func TestNewTargetUnknownKind(t *testing.T) {
	if _, err := NewTarget(config.TargetConfig{Name: "x", Kind: "bogus"}); err == nil {
		t.Error("expected error for unknown kind")
	}
}

func TestNewRegistryValidation(t *testing.T) {
	t.Run("duplicate name", func(t *testing.T) {
		_, err := NewRegistry([]config.TargetConfig{
			{Name: "a", Kind: "docker", Address: "tcp://x:1"},
			{Name: "a", Kind: "docker", Address: "tcp://y:1"},
		})
		if err == nil {
			t.Error("expected duplicate-name error")
		}
	})
	t.Run("missing name", func(t *testing.T) {
		_, err := NewRegistry([]config.TargetConfig{{Kind: "docker", Address: "tcp://x:1"}})
		if err == nil {
			t.Error("expected missing-name error")
		}
	})
}

func TestDistributorFanout(t *testing.T) {
	good := &fakeTarget{name: "good", pull: func() error { return nil }}
	bad := &fakeTarget{name: "bad", pull: func() error { return errors.New("boom") }}
	d := NewDistributor(registryOf(good, bad), "")

	store := warm.NewMemStore()
	job := warm.NewJob("j", "docker.io/x:1", "cache.local/x:1", nil, time.Now())
	_ = store.Add(job)
	d.Distribute(context.Background(), job, store)

	snap, _ := store.Snapshot("j")
	states := map[string]warm.TargetSnapshot{}
	for _, tp := range snap.Targets {
		states[tp.Name] = tp
	}
	if len(states) != 2 {
		t.Fatalf("targets = %+v", snap.Targets)
	}
	if states["good"].State != "pulled" {
		t.Errorf("good = %+v", states["good"])
	}
	if states["bad"].State != "failed" || states["bad"].Err == "" {
		t.Errorf("bad = %+v", states["bad"])
	}
}

func TestDistributorUnknownTarget(t *testing.T) {
	d := NewDistributor(registryOf(), "")
	store := warm.NewMemStore()
	job := warm.NewJob("j", "docker.io/x:1", "cache.local/x:1", nil, time.Now())
	job.SetRequestedTargets([]string{"nope"})
	_ = store.Add(job)
	d.Distribute(context.Background(), job, store)

	snap, _ := store.Snapshot("j")
	if len(snap.Targets) != 1 || snap.Targets[0].State != "failed" {
		t.Fatalf("targets = %+v", snap.Targets)
	}
}

func TestRewriteHost(t *testing.T) {
	const dig = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	cases := []struct{ ref, host, want string }{
		{"192.168.0.22:5000/library/redis:7", "cache.cr.com", "cache.cr.com/library/redis:7"},
		{"192.168.0.22:5000/team/app@" + dig, "cache.cr.com:5000", "cache.cr.com:5000/team/app@" + dig},
	}
	for _, c := range cases {
		got, err := rewriteHost(c.ref, c.host)
		if err != nil {
			t.Fatalf("rewriteHost(%q,%q): %v", c.ref, c.host, err)
		}
		if got != c.want {
			t.Errorf("rewriteHost(%q,%q) = %q, want %q", c.ref, c.host, got, c.want)
		}
	}
}

func TestDistributorHostOverride(t *testing.T) {
	a := &fakeTarget{name: "a"} // no pull_host -> global default
	b := &fakeTarget{name: "b"} // per-target pull_host wins
	reg := &Registry{byName: map[string]Target{"a": a, "b": b}}
	reg.entries = []entry{
		{cfg: config.TargetConfig{Name: "a", Kind: "fake"}, target: a},
		{cfg: config.TargetConfig{Name: "b", Kind: "fake", PullHost: "b.internal:5000"}, target: b},
	}
	d := NewDistributor(reg, "cache.cr.com")

	store := warm.NewMemStore()
	job := warm.NewJob("j", "docker.io/library/redis:7", "192.168.0.22:5000/library/redis:7", nil, time.Now())
	_ = store.Add(job)
	d.Distribute(context.Background(), job, store)

	if a.pulled != "cache.cr.com/library/redis:7" {
		t.Errorf("a pulled %q, want cache.cr.com/library/redis:7", a.pulled)
	}
	if b.pulled != "b.internal:5000/library/redis:7" {
		t.Errorf("b pulled %q, want b.internal:5000/library/redis:7", b.pulled)
	}
	snap, _ := store.Snapshot("j")
	refs := map[string]string{}
	for _, tp := range snap.Targets {
		if tp.State != "pulled" {
			t.Errorf("target %s state = %q, want pulled", tp.Name, tp.State)
		}
		refs[tp.Name] = tp.Ref
	}
	if refs["a"] != "cache.cr.com/library/redis:7" || refs["b"] != "b.internal:5000/library/redis:7" {
		t.Errorf("snapshot refs = %v", refs)
	}
}
