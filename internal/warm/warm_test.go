package warm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/store"
)

func newWarmer(t *testing.T, stores []config.StoreConfig, allowUnknown bool) (*Warmer, Store) {
	t.Helper()
	var c config.Config
	c.Serve.Stores = stores
	c.Serve.AllowUnknownStores = allowUnknown
	c.Serve.Warm = config.WarmConfig{MaxConcurrentJobs: 2, MaxConcurrentLayers: 2, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Serve.Stores, c.Serve.AllowUnknownStores)
	if err != nil {
		t.Fatal(err)
	}
	js := NewMemStore()
	w := NewWarmer(set, js, c.Serve.Warm)
	w.srcOpts = []name.Option{name.Insecure}
	return w, js
}

func waitTerminal(t *testing.T, js Store, id string) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := js.Snapshot(id); ok && snap.State.Terminal() {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting for terminal state")
	return JobSnapshot{}
}

func TestWarmerCopyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	up := startRegistry(t)
	cache := startRegistry(t)
	pushImage(t, up+"/team/app:1", 3)

	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "up", Kind: "registry", Host: up, Insecure: true},
		{Name: "cache", Kind: "registry", Host: cache, Insecure: true, Mode: "copy"},
	}, false)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, err := w.Submit(Request{Ref: "team/app:1", From: "up", To: "cache", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if len(done.Transfers) != 1 {
		t.Fatalf("transfers = %d, want 1", len(done.Transfers))
	}
	tr := done.Transfers[0]
	if tr.Store != "cache" || tr.From != "up" || tr.Kind != "registry" {
		t.Errorf("transfer = %+v", tr)
	}
	if tr.BytesTotal == 0 || tr.BytesDone != tr.BytesTotal {
		t.Errorf("bytes = %d/%d", tr.BytesDone, tr.BytesTotal)
	}
	dst, err := name.ParseReference(tr.Ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(dst); err != nil {
		t.Errorf("cache ref %q not resolvable: %v", tr.Ref, err)
	}
}

func TestWarmerDedup(t *testing.T) {
	w, _ := newWarmer(t, []config.StoreConfig{
		{Name: "cache", Kind: "registry", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background() // enqueue without starting workers; jobs stay active
	req := Request{Ref: "team/app:1", From: "docker.io", To: "cache", Platforms: []string{"linux/amd64"}}
	s1, err := w.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := w.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != s2.ID {
		t.Errorf("expected dedup onto one job, got %s and %s", s1.ID, s2.ID)
	}
}

func TestWarmerQueueFull(t *testing.T) {
	w, _ := newWarmer(t, []config.StoreConfig{
		{Name: "cache", Kind: "registry", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background()
	w.jobs = make(chan *Job, 1)
	if _, err := w.Submit(Request{Ref: "a/x:1", From: "r.io", To: "cache"}); err != nil {
		t.Fatal(err)
	}
	_, err := w.Submit(Request{Ref: "b/y:1", From: "r.io", To: "cache"})
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestWarmerNothingToDo(t *testing.T) {
	w, _ := newWarmer(t, nil, true)
	w.base = context.Background()
	if _, err := w.Submit(Request{Ref: "x/y:1", From: "r.io"}); err == nil {
		t.Error("expected error when neither to nor distribute is set")
	}
}
