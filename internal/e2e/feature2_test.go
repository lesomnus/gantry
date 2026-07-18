package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Feature 4: platform selection — Plan echoes the narrowed platform and a
// narrowed copy succeeds.
func TestPlatformSelection(t *testing.T) {
	h := newHarness(t)
	seedPlatformIndex(t, h.remote, "lib/multi", "1", "linux/amd64", "linux/arm64")

	res, err := h.client.Job().Plan(context.Background(), pb.JobPlanRequest_builder{
		Ref: proto.String("lib/multi:1"), Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache"),
		Platforms: []string{"linux/amd64"},
	}.Build())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if got := res.GetPlatforms(); len(got) != 1 || got[0] != "linux/amd64" {
		t.Errorf("plan platforms = %v, want [linux/amd64]", got)
	}

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref: "lib/multi:1", Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache"),
		Platforms: []string{"linux/amd64"},
	}.Build()).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("narrowed copy state=%v error=%q", job.GetState(), job.GetError())
	}
}

// Feature 5: caller-chosen `as` names are what the engine records.
func TestAsNames(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")
	h.waitDone(h.add(copyReq("remote", "cache")).GetId())

	const asName = "docker.io/lib/app:1"
	h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref: "lib/app:1", Source: pb.StoreByName("cache"), Target: pb.StoreByName("edge"),
		As: []string{asName},
	}.Build()).GetId())

	if !h.engine.has(asName) {
		t.Errorf("engine did not record the `as` name %q", asName)
	}
	if last := h.engine.lastPull(); len(last.As) != 1 || last.As[0] != asName {
		t.Errorf("pull recorded as = %v, want [%s]", last.As, asName)
	}
}

// Feature 6: a digest-pinned job commits the source verbatim and refuses
// platform narrowing.
func TestDigestPin(t *testing.T) {
	h := newHarness(t)
	d := seedImage(t, h.remote, "lib/app", "1")
	ref := "lib/app@" + d.String()

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref: ref, Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache"),
	}.Build()).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("digest copy state=%v error=%q", job.GetState(), job.GetError())
	}

	// The same digest resolves from the cache (verbatim commit).
	got, err := digestByRef(t, h.cache, "lib/app", d.String())
	if err != nil {
		t.Fatalf("cache missing the pinned digest: %v", err)
	}
	if got.String() != d.String() {
		t.Errorf("cache digest = %s, want %s", got, d)
	}

	// A digest ref refuses platform narrowing (verbatim, all-platforms).
	_, err = h.client.Job().Plan(context.Background(), pb.JobPlanRequest_builder{
		Ref: proto.String(ref), Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache"),
		Platforms: []string{"linux/amd64"},
	}.Build())
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("digest + platforms error = %v, want InvalidArgument", err)
	}
}

// Feature 11: Retry re-submits a terminal job as a fresh one; Cancel of a
// terminal job is rejected.
func TestCancelRetry(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")
	id := h.waitDone(h.add(copyReq("remote", "cache")).GetId()).GetId()

	rj, err := h.client.Job().Retry(context.Background(), pb.JobById(id))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if rj.GetId() == id {
		t.Error("retry returned the same job id, want a fresh one")
	}
	if s := h.waitDone(rj.GetId()).GetState(); s != pb.JobState_JOB_STATE_DONE {
		t.Errorf("retried job state = %v", s)
	}

	if _, err := h.client.Job().Cancel(context.Background(), pb.JobById(id)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("cancel of a terminal job error = %v, want FailedPrecondition", err)
	}
}

// Feature 9: retention GC deletes an idle image once its age passes max_idle,
// driven entirely by the injected clock — no wall-clock waits.
func TestRetentionGC(t *testing.T) {
	idle := config.Duration(time.Hour)
	h := newHarness(t, withRules(config.RetentionRule{Repo: "**", MaxIdle: &idle}))
	seedImage(t, h.remote, "lib/app", "1")
	h.waitDone(h.add(copyReq("remote", "cache")).GetId())
	h.waitDone(h.add(copyReq("cache", "edge")).GetId()) // stamps the retention index

	if len(h.imageList("edge")) == 0 {
		t.Fatal("no retention records after the engine pull")
	}

	h.clock.advance(1000 * time.Hour)
	res := h.gcApply("edge")
	if len(res.GetDeleted()) == 0 && len(res.GetUntagged()) == 0 {
		t.Errorf("gc apply removed nothing after aging past max_idle: %+v", res)
	}
	if n := len(h.imageList("edge")); n != 0 {
		t.Errorf("record survived idle GC: %d remain", n)
	}
}
