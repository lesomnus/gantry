package cpx

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
)

// fakeDeliveries is a DeliveryHook that tracks a fixed set of stores.
type fakeDeliveries struct {
	tracked []string
	mu      sync.Mutex
	got     []string // "store repo@digest:tag"
}

func (f *fakeDeliveries) Tracks(store string) bool { return slices.Contains(f.tracked, store) }

func (f *fakeDeliveries) Delivered(store, repo, digest, tag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, store+" "+repo+"@"+digest+":"+tag)
}

func (f *fakeDeliveries) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func deliveryCopier(t *testing.T, hook *fakeDeliveries) (w *Copier, js Store, cloud, site string) {
	t.Helper()
	cloud, site = startRegistry(t), startRegistry(t)
	w, js = newCopier(t, []config.StoreConfig{
		{Name: "cloud", Kind: "oci", Host: cloud, Insecure: true, Cache: "site"},
		{Name: "site", Kind: "oci", Host: site, Insecure: true, Mode: "copy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, &fakePullEngine{name: "node", platform: "linux/amd64"})
	w.SetDeliveryHook(hook)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })
	return w, js, cloud, site
}

func submitDone(t *testing.T, w *Copier, js Store, req Request) JobSnapshot {
	t.Helper()
	snap, _, err := w.Submit(req)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	return done
}

// A routed engine job delivers through the cache twice over: the fill gantry
// added puts the image there, and the node's pull reads it back out. Both are
// recorded, under the job's tag and the digest the authority settled; the
// origin, which nobody tracks, records nothing.
func TestARoutedEngineJobRecordsItsDeliveriesThroughTheCache(t *testing.T) {
	hook := &fakeDeliveries{tracked: []string{"site"}}
	w, js, cloud, _ := deliveryCopier(t, hook)
	ref := pushImage(t, cloud+"/team/app:1", 1)
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatal(err)
	}
	want := "site team/app@" + desc.Digest.String() + ":1"

	submitDone(t, w, js, Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "node"})

	got := hook.all()
	if len(got) == 0 {
		t.Fatal("nothing recorded")
	}
	for _, g := range got {
		if g != want {
			t.Errorf("recorded %q, want only %q", g, want)
		}
	}
	// The fill, the pull's start and its end.
	if len(got) != 3 {
		t.Errorf("recorded %d deliveries %v, want the fill and both ends of the pull", len(got), got)
	}
}

// A copy the caller asked for into the cache is the caller's; the registry's
// retention does not take it back out, so it is not recorded.
func TestACopyIntoTheCacheIsNotADelivery(t *testing.T) {
	hook := &fakeDeliveries{tracked: []string{"site"}}
	w, js, cloud, _ := deliveryCopier(t, hook)
	pushImage(t, cloud+"/team/app:1", 1)

	submitDone(t, w, js, Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "site"})

	if got := hook.all(); len(got) != 0 {
		t.Errorf("recorded %v, want nothing", got)
	}
}

// A pull that names a tag and reads the tracked store directly has no digest
// on the plan; what the tag resolved to is asked of the store, so the delivery
// is still recorded by digest.
func TestAnUnanchoredPullFromATrackedStoreIsRecordedByDigest(t *testing.T) {
	hook := &fakeDeliveries{tracked: []string{"site"}}
	w, js, _, site := deliveryCopier(t, hook)
	ref := pushImage(t, site+"/team/app:1", 1)
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatal(err)
	}

	submitDone(t, w, js, Request{Ref: site + "/team/app:1", Source: "site", Target: "node"})

	want := "site team/app@" + desc.Digest.String() + ":1"
	if got := hook.all(); !slices.Contains(got, want) {
		t.Errorf("recorded %v, want %q", got, want)
	}
}

// A store nobody tracks is never asked anything extra.
func TestAnUntrackedStoreRecordsNothing(t *testing.T) {
	hook := &fakeDeliveries{}
	w, js, _, site := deliveryCopier(t, hook)
	pushImage(t, site+"/team/app:1", 1)

	submitDone(t, w, js, Request{Ref: site + "/team/app:1", Source: "site", Target: "node"})

	if got := hook.all(); len(got) != 0 {
		t.Errorf("recorded %v, want nothing", got)
	}
}
