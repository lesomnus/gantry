//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/lesomnus/gantry/pb"
)

// TestL2CopyAndEnginePull drives the full remote→cache→daemon path against real
// registry:2 containers and the real docker daemon: gantry copies into the cache,
// then the daemon pulls it back out.
func TestL2CopyAndEnginePull(t *testing.T) {
	h := newL2Harness(t)
	seedImage(t, h.remote, "lib/app", "1")

	// Registry→registry copy into a real cache.
	if j := h.waitDone(h.add(copyReq("remote", "cache")).GetId()); j.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("copy state=%v error=%q", j.GetState(), j.GetError())
	}
	if !hasTag(t, h.cache, "lib/app", "1") {
		t.Fatal("cache is missing lib/app:1 after copy")
	}

	// The real docker daemon pulls from the cache.
	pullRef := h.cache + "/lib/app:1"
	h.removeImage(pullRef)
	if j := h.waitDone(h.add(copyReq("cache", "edge")).GetId()); j.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("engine pull state=%v error=%q", j.GetState(), j.GetError())
	}
	if _, err := h.cli.ImageInspect(context.Background(), pullRef); err != nil {
		t.Errorf("daemon is missing the pulled image %s: %v", pullRef, err)
	}
}
