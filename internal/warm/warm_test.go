package warm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
)

func newWarmer(t *testing.T, cacheHost string) (*Warmer, Store) {
	t.Helper()
	var c config.Config
	c.Serve.Registry = config.RegistryConfig{Mode: "copy", Host: cacheHost, Insecure: true}
	c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 2, MaxConcurrentLayers: 2, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	src, err := NewSource(c.Serve.Registry)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	w := NewWarmer(src, store, c.Serve.Registry, c.Serve.Warm)
	w.srcOpts = []name.Option{name.Insecure}
	return w, store
}

func waitTerminal(t *testing.T, store Store, id string) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := store.Snapshot(id); ok && snap.State.Terminal() {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting for terminal state")
	return JobSnapshot{}
}

func TestWarmerEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	up := startRegistry(t)
	cache := startRegistry(t)
	pushImage(t, up+"/team/app:1", 3)

	w, store := newWarmer(t, cache)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, err := w.Submit(Request{Ref: up + "/team/app:1", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	done := waitTerminal(t, store, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if done.BytesTotal == 0 || done.BytesDone != done.BytesTotal {
		t.Errorf("bytes = %d/%d", done.BytesDone, done.BytesTotal)
	}
	for _, l := range done.Layers {
		if l.State != "warm" {
			t.Errorf("layer %s state = %q, want warm", l.Digest, l.State)
		}
	}

	dst, err := name.ParseReference(done.CacheRef, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(dst); err != nil {
		t.Errorf("cache tag %q not resolvable: %v", done.CacheRef, err)
	}
}

func TestWarmerDedup(t *testing.T) {
	w, _ := newWarmer(t, "cache.local")
	w.base = context.Background() // enqueue without starting workers; jobs stay active
	s1, err := w.Submit(Request{Ref: "example.com/team/app:1", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := w.Submit(Request{Ref: "example.com/team/app:1", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != s2.ID {
		t.Errorf("expected dedup onto one job, got %s and %s", s1.ID, s2.ID)
	}
}

func TestWarmerQueueFull(t *testing.T) {
	w, _ := newWarmer(t, "cache.local")
	w.base = context.Background()
	w.jobs = make(chan *Job, 1)
	if _, err := w.Submit(Request{Ref: "example.com/lib/a:1"}); err != nil {
		t.Fatal(err)
	}
	_, err := w.Submit(Request{Ref: "example.com/lib/b:1"})
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}
