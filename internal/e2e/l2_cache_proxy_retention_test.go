//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/protobuf/proto"
)

// TestL2CacheDropsWhatTheEngineDropped with the cache a PULL-THROUGH, which is
// what a robot's is (hday-os release/base/registry/registry.yaml: `dist/**` and
// `stage/**` are proxy prefixes, everything else is a repository the registry
// owns).
//
// It is a separate test and not a mode flag on that one, because two things
// differ and both are the point.
//
// **The delete takes another path.** cr refuses writes to a proxy prefix --
// 405 on the upload routes and on a manifest PUT -- and lets DELETE through.
// Nothing here exercised that: every other cache test seeds by PUSHING into
// the cache, which a proxy prefix would refuse, so `cache` has always been an
// ordinary registry. A robot's cache is never one.
//
// **"It is gone" cannot be read the same way.** ASKING A PULL-THROUGH IS
// FETCHING FROM IT: `digestByRef` against a proxy repository answers 200 by
// going and getting it, so polling for a 404 after a delete would refill the
// cache and then fail, or pass for the wrong reason on a slow upstream. The
// origin is killed first, so an answer can only come from what the cache kept.
func TestL2ProxyCacheDropsWhatTheEngineDropped(t *testing.T) {
	h := newL2Harness(t,
		l2WithRemoteCache("cache"),
		l2WithProxyCache(),
		l2WithRetention(config.RetentionRule{Repo: "{remote}/lib/**"}),
		l2WithCacheRetention(),
	)
	ctx := context.Background()
	dg := seedImage(t, h.remote, "lib/app", "1").String()
	h.removeImage(h.cache + "/lib/app:1")

	job := h.waitDone(h.add(pullJob(h.remote+"/lib/app:1", "remote", false)).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q [%s]", job.GetState(), job.GetError(), describe(job))
	}

	cacheRef := "lib/app@" + dg
	reason := func() string {
		plan, err := h.client.Store().GcPlan(ctx, pb.StoreGcRequest_builder{Store: pb.StoreByName("cache")}.Build())
		if err != nil {
			t.Fatalf("cache gc plan: %v", err)
		}
		for _, c := range plan.GetDelete() {
			if c.GetRef() == cacheRef {
				return c.GetReason().String()
			}
		}
		for _, k := range plan.GetKeep() {
			if k.GetRef() == cacheRef {
				return k.GetReason().String()
			}
		}
		return "absent"
	}
	if got := reason(); got != pb.GcKeepReason_GC_KEEP_REASON_HELD_BY_ENGINE.String() {
		t.Fatalf("while the engine holds it the cache plan says %s", got)
	}

	// From here the cache is on its own. Reading it can no longer refill it,
	// so a 200 is content it kept and a failure is content it does not have --
	// which is the only way to tell a delete from a delete-and-refetch.
	h.kill(h.remoteID, h.remote)
	if _, err := digestByRef(t, h.cache, "lib/app", dg); err != nil {
		t.Fatalf("with the origin down the cache does not answer for %s; the routed pull left nothing in it: %v", dg, err)
	}

	// The engine drops it, the way its own GC would.
	imgs, err := h.client.Image().List(ctx, pb.ImageListRequest_builder{
		Store: pb.StoreByName("edge"),
		Repo:  proto.String(h.cache + "/lib/app"),
	}.Build())
	if err != nil {
		t.Fatalf("edge images: %v", err)
	}
	if len(imgs.GetItems()) == 0 {
		t.Fatal("the engine's index holds nothing under the cache; the routed pull was not recorded")
	}
	for _, img := range imgs.GetItems() {
		if _, err := h.client.Store().Remove(ctx, pb.StoreRemoveRequest_builder{
			Store: pb.StoreByName("edge"),
			Ref:   proto.String(img.GetRef()),
		}.Build()); err != nil {
			t.Fatalf("remove %s from the engine: %v", img.GetRef(), err)
		}
	}

	// Nobody asks the cache: the drop wakes its scheduler. cr is the only
	// registry that reaches here (crProxyConfig skips the others), and cr
	// deletes.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := digestByRef(t, h.cache, "lib/app", dg); err != nil {
			t.Logf("the pull-through dropped %s on its own", cacheRef)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the pull-through still holds %s after the engine dropped it; its plan says %s", cacheRef, reason())
}
