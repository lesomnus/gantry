package e2e

import (
	"sync/atomic"
	"testing"

	"github.com/lesomnus/gantry/pb"
)

func copyReq(source, target string) *pb.JobAddRequest {
	return pb.JobAddRequest_builder{
		Ref:    "lib/app:1",
		Source: pb.StoreByName(source),
		Target: pb.StoreByName(target),
	}.Build()
}

// Feature 1: registry→registry copy, and incremental blob skip on re-copy.
func TestCopyRemoteToCache(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")

	before := atomic.LoadInt32(h.cacheUploads)
	job := h.waitDone(h.add(copyReq("remote", "cache")).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q", job.GetState(), job.GetError())
	}
	if !hasTag(t, h.cache, "lib/app", "1") {
		t.Fatal("cache is missing lib/app:1 after copy")
	}
	if first := atomic.LoadInt32(h.cacheUploads) - before; first == 0 {
		t.Fatal("expected blob uploads on the first copy")
	}

	// A second identical copy must upload zero blobs — every blob is already present.
	mid := atomic.LoadInt32(h.cacheUploads)
	h.waitDone(h.add(copyReq("remote", "cache")).GetId())
	if delta := atomic.LoadInt32(h.cacheUploads) - mid; delta != 0 {
		t.Errorf("second copy uploaded %d blobs, want 0 (incremental skip)", delta)
	}
}

// Feature 2 (hermetic half): the engine is told to pull from the cache.
func TestEnginePull(t *testing.T) {
	h := newHarness(t)
	seedImage(t, h.remote, "lib/app", "1")
	h.waitDone(h.add(copyReq("remote", "cache")).GetId()) // fill the cache first

	job := h.waitDone(h.add(copyReq("cache", "edge")).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q", job.GetState(), job.GetError())
	}
	if h.engine.pullCount() != 1 {
		t.Fatalf("engine pulls = %d, want 1", h.engine.pullCount())
	}
}
