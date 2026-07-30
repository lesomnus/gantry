package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/lesomnus/gantry/pb"
	"google.golang.org/protobuf/proto"
)

// A cache that cannot serve the image does not fail the node's pull when the job
// allows a fallback: the engine is pointed at the origin named in the job's own
// ref, the job completes, and the failed attempt stays on the record.
//
// Nothing fills the cache here — this is the shape a caller lands in when the
// cache-fill job failed, has not run yet, or the cache is unreachable, all of
// which are the same fact from the pull's side.
func TestEnginePullFallsBackToOrigin(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")
	if hasTag(t, h.cache, "lib/app", "1") {
		t.Fatal("the cache must be empty for this test to mean anything")
	}
	h.engine.failFor = func(ref string) error {
		if strings.HasPrefix(ref, h.cache+"/") {
			return errMissingFromCache
		}
		return nil
	}

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:              h.remote + "/lib/app:1",
		Source:           pb.StoreByName("cache"),
		Target:           pb.StoreByName("edge"),
		FallbackToOrigin: proto.Bool(true),
	}.Build()).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q, want a done job — a cache miss is not a job failure",
			job.GetState(), job.GetError())
	}
	transfers := job.GetTransfers()
	if len(transfers) != 2 {
		t.Fatalf("transfers = %d, want one per attempt", len(transfers))
	}
	if got := transfers[0]; got.GetState() != pb.TransferState_TRANSFER_STATE_FAILED ||
		got.GetSource() != "cache" || got.GetError() == "" {
		t.Errorf("transfer[0] = %+v, want the failed cache attempt with its error", got)
	}
	if got := transfers[1]; got.GetState() != pb.TransferState_TRANSFER_STATE_DONE ||
		got.GetSource() != "remote" {
		t.Errorf("transfer[1] = %+v, want the origin attempt to have served the image", got)
	}
	// The job's stores are what it was ADMITTED for. Where the bytes came from is
	// a property of an attempt, and this job had two — asserted above.
	if got := job.GetSource().GetName(); got != "cache" {
		t.Errorf("job source = %q, want the requested source", got)
	}
	if got := job.GetTarget().GetName(); got != "edge" {
		t.Errorf("job target = %q, want the requested target", got)
	}
	if !h.engine.has(h.remote + "/lib/app:1") {
		t.Error("the engine should hold the image under the origin ref")
	}
}

// Without the flag the same setup fails, and only the source it was given is
// ever contacted.
func TestEnginePullWithoutFallbackDoesNotTouchTheOrigin(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")
	h.engine.failFor = func(ref string) error {
		if strings.HasPrefix(ref, h.cache+"/") {
			return errMissingFromCache
		}
		return nil
	}

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:    h.remote + "/lib/app:1",
		Source: pb.StoreByName("cache"),
		Target: pb.StoreByName("edge"),
	}.Build()).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("state=%v, want failed", job.GetState())
	}
	if n := len(job.GetTransfers()); n != 1 {
		t.Errorf("transfers = %d, want 1 — no second attempt was permitted", n)
	}
	for _, p := range h.engine.pullRecords() {
		if strings.HasPrefix(p.Ref, h.remote+"/") {
			t.Errorf("the engine was sent to the origin %q without the flag", p.Ref)
		}
	}
}

type cacheMissError struct{}

func (cacheMissError) Error() string { return "MANIFEST_UNKNOWN: manifest unknown" }

var errMissingFromCache = cacheMissError{}

// A job reports the stores it was admitted for whatever happened to it — here,
// nothing served it at all.
func TestFailedFallbackReportsTheRequestedSource(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")
	h.engine.failFor = func(string) error { return errMissingFromCache }

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:              h.remote + "/lib/app:1",
		Source:           pb.StoreByName("cache"),
		Target:           pb.StoreByName("edge"),
		FallbackToOrigin: proto.Bool(true),
	}.Build()).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_FAILED {
		t.Fatalf("state=%v, want failed when no source could serve it", job.GetState())
	}
	if n := len(job.GetTransfers()); n != 2 {
		t.Errorf("transfers = %d, want both attempts on the record", n)
	}
	if got := job.GetSource().GetName(); got != "cache" {
		t.Errorf("job source = %q, want the requested source for a job nothing served", got)
	}
	if !strings.Contains(job.GetError(), "cache") || !strings.Contains(job.GetError(), "remote") {
		t.Errorf("job error = %q, want both attempts' failures reported", job.GetError())
	}
}

// A copy through a cache, end to end over the API: the remote is read once, the
// site is filled, and the caller's target is served from the site — all as one job
// whose hops are its transfers.
func TestRoutedCopyThroughACache(t *testing.T) {
	h := newHarness(t, withRemoteCache("cache"))
	seedImage(t, h.remote, "lib/app", "1")

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:    h.remote + "/lib/app:1",
		Source: pb.StoreByName("remote"),
		Target: pb.StoreByName("edge"),
	}.Build()).GetId())

	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q", job.GetState(), job.GetError())
	}
	transfers := job.GetTransfers()
	if len(transfers) != 2 {
		t.Fatalf("transfers = %d, want a fill hop and a delivery hop", len(transfers))
	}
	if fill := transfers[0]; fill.GetStep() != 0 || fill.GetStore() != "cache" ||
		fill.GetSource() != "remote" || fill.GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("hop 0 = %+v, want cache ◀── remote", fill)
	}
	if deliver := transfers[1]; deliver.GetStep() != 1 || deliver.GetStore() != "edge" ||
		deliver.GetSource() != "cache" || deliver.GetState() != pb.TransferState_TRANSFER_STATE_DONE {
		t.Errorf("hop 1 = %+v, want edge ◀── cache", deliver)
	}
	// The job still reports what the caller asked for.
	if job.GetSource().GetName() != "remote" || job.GetTarget().GetName() != "edge" {
		t.Errorf("job stores = %q -> %q, want remote -> edge",
			job.GetSource().GetName(), job.GetTarget().GetName())
	}
	// The cache holds it, so the node was fed from there.
	if !hasTag(t, h.cache, "lib/app", "1") {
		t.Error("the cache should hold the image after the fill hop")
	}
	if !h.engine.has(h.cache + "/lib/app:1") {
		t.Error("the engine should have been sent to the cache")
	}
}

// Plan reports the route before anything runs, so an operator can see that a job
// will go through a cache without submitting it.
func TestPlanReportsTheRoute(t *testing.T) {
	h := newHarness(t, withRemoteCache("cache"))
	seedImage(t, h.remote, "lib/app", "1")

	res, err := h.client.Job().Plan(context.Background(), pb.JobPlanRequest_builder{
		Ref:    proto.String(h.remote + "/lib/app:1"),
		Source: pb.StoreByName("remote"),
		Target: pb.StoreByName("edge"),
	}.Build())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	steps := res.GetSteps()
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want a fill and a delivery", len(steps))
	}
	if steps[0].GetStore() != "cache" || !steps[0].GetOptional() {
		t.Errorf("step 0 = %+v, want an optional fill of the cache", steps[0])
	}
	if got := steps[0].GetSources(); len(got) != 1 || got[0].GetStore() != "remote" ||
		got[0].GetWhy() != pb.PlanSourceWhy_PLAN_SOURCE_WHY_PLANNED {
		t.Errorf("step 0 sources = %+v, want the remote alone", got)
	}
	if steps[1].GetStore() != "edge" || steps[1].GetOptional() {
		t.Errorf("step 1 = %+v, want the requested delivery", steps[1])
	}
	got := steps[1].GetSources()
	if len(got) != 2 {
		t.Fatalf("step 1 sources = %+v, want the cache then the remote", got)
	}
	if got[0].GetStore() != "cache" || got[0].GetWhy() != pb.PlanSourceWhy_PLAN_SOURCE_WHY_ROUTE {
		t.Errorf("first source = %+v, want the cache as the route", got[0])
	}
	if got[1].GetStore() != "remote" || got[1].GetWhy() != pb.PlanSourceWhy_PLAN_SOURCE_WHY_PLANNED {
		t.Errorf("second source = %+v, want the remote as the source the caller named", got[1])
	}
	// A dry run moves nothing.
	if hasTag(t, h.cache, "lib/app", "1") {
		t.Error("Plan filled the cache; it must be a dry run")
	}
}
