package cpx

import (
	"context"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
)

// A copy into a registry that the registry has since lost -- a GC that
// collects untagged manifests, a hand -- is not still delivered. Asked by the
// digest the job committed, not its tag.
func TestStillDeliveredAsksTheRegistryForTheDigest(t *testing.T) {
	w, js, cloud, site := deliveryCopier(t, &fakeDeliveries{})
	pushImage(t, cloud+"/team/app:1", 1)
	done := submitDone(t, w, js, Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "site"})

	if !w.StillDelivered(context.Background(), done) {
		t.Fatal("a copy that is in its target reads as gone")
	}

	tr, _ := finalTransfer(done)
	dg, err := name.NewDigest(site+"/team/app@"+tr.Digest, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Delete(dg); err != nil {
		t.Fatal(err)
	}
	if w.StillDelivered(context.Background(), done) {
		t.Error("a copy its registry deleted still reads as delivered")
	}
}

// Only a definite "not here" re-runs a job. A target that cannot be asked
// replays as before, and so does anything that did not end DONE.
func TestStillDeliveredReplaysOnDoubt(t *testing.T) {
	w, js, cloud, _ := deliveryCopier(t, &fakeDeliveries{})
	pushImage(t, cloud+"/team/app:1", 1)
	done := submitDone(t, w, js, Request{Ref: cloud + "/team/app:1", Source: "cloud", Target: "site"})

	unknown := done
	unknown.Transfers = append([]TransferSnapshot(nil), done.Transfers...)
	unknown.Transfers[len(unknown.Transfers)-1].Store = "nowhere"
	if !w.StillDelivered(context.Background(), unknown) {
		t.Error("a target that cannot be asked re-ran the job")
	}

	failed := done
	failed.State = JobFailed
	if !w.StillDelivered(context.Background(), failed) {
		t.Error("a failed job was checked; only DONE is")
	}
}

// holdingEngine is a pull engine that can say which names it holds.
type holdingEngine struct {
	*fakePullEngine
	mu   sync.Mutex
	gone map[string]bool
}

func (h *holdingEngine) Has(_ context.Context, ref string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.gone[ref], nil
}

// An engine delivery is still there while the engine has every name the job
// left on it: the `as` names when there were any.
func TestStillDeliveredAsksTheEngineForTheNames(t *testing.T) {
	eng := &holdingEngine{fakePullEngine: &fakePullEngine{name: "node", platform: "linux/amd64"}, gone: map[string]bool{}}
	up := startRegistry(t)
	w, js := newCopier(t, []config.StoreConfig{{Name: "up", Kind: "oci", Host: up, Insecure: true}}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })
	pushImage(t, up+"/team/app:1", 1)

	done := submitDone(t, w, js, Request{Ref: "team/app:1", Source: "up", Target: "node", As: []string{"docker.io/team/app:1"}})
	if !w.StillDelivered(context.Background(), done) {
		t.Fatal("an image the engine holds reads as gone")
	}
	eng.mu.Lock()
	eng.gone["docker.io/team/app:1"] = true
	eng.mu.Unlock()
	if w.StillDelivered(context.Background(), done) {
		t.Error("an image the engine no longer holds still reads as delivered")
	}
}

// An engine that cannot say what it holds replays as before.
func TestStillDeliveredReplaysForAnEngineThatCannotSay(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, up := engineCopier(t, eng)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })
	pushImage(t, up+"/team/app:1", 1)

	done := submitDone(t, w, js, Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if !w.StillDelivered(context.Background(), done) {
		t.Error("an engine without Holder re-ran the job")
	}
}
