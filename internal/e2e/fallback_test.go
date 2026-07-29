package e2e

import (
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
	// The job reports the source that actually served it, not the one it tried first.
	if got := job.GetSource().GetName(); got != "remote" {
		t.Errorf("job source = %q, want the store the image came from", got)
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

// A job that no source could serve reports the source it was POINTED at, not
// the last one it tried — the job failed, nothing served it, and naming the
// fallback would read as though the operator had asked for it.
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
