//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/protobuf/proto"
)

// The cache follows the engine, end to end, against a real daemon and a real
// registry: a routed pull leaves the image in the cache, held there while the
// engine's retention holds it; once the engine drops it, the cache's scheduler
// — woken by that drop, not asked — deletes it from the registry.
//
// Whether the registry deletes is its own configuration (distribution 2.x
// ships with deletes off). There, the pass has to report the refusal and leave
// the manifest and its record alone for the next pass. cr, the registry this
// exists for, has to delete.
func TestL2CacheDropsWhatTheEngineDropped(t *testing.T) {
	h := newL2Harness(t,
		l2WithRemoteCache("cache"),
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
	if _, err := digestByRef(t, h.cache, "lib/app", dg); err != nil {
		t.Fatalf("the routed pull left nothing in the cache: %v", err)
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

	// The engine drops it — through the index, the way its own GC would. Only
	// what this test pulled: the daemon is shared, and other runs' images under
	// other hosts are none of this cache's business.
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

	// Nobody asks the cache: the drop wakes its scheduler.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := digestByRef(t, h.cache, "lib/app", dg); err != nil {
			t.Logf("%s: the cache dropped %s on its own", registryImage(), cacheRef)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Still there. That is only right if the registry refuses deletes, and then
	// the refusal is what an apply says and the manifest is still a candidate.
	if isCR(registryImage()) {
		t.Fatalf("cr still holds %s after the engine dropped it", cacheRef)
	}
	if got := reason(); got != pb.GcDeleteReason_GC_DELETE_REASON_DROPPED_BY_ENGINES.String() {
		t.Fatalf("the cache still holds %s and its plan says %s, want it dropped by the engines", cacheRef, got)
	}
	res, err := h.client.Store().GcApply(ctx, pb.StoreGcRequest_builder{Store: pb.StoreByName("cache")}.Build())
	if err != nil {
		t.Fatalf("cache gc apply: %v", err)
	}
	if len(res.GetErrors()) == 0 || !strings.Contains(strings.Join(res.GetErrors(), "\n"), "does not allow") {
		t.Fatalf("the cache kept %s without saying why: %v", cacheRef, res.GetErrors())
	}
	t.Logf("%s refuses deletes: %v", registryImage(), res.GetErrors())
}
