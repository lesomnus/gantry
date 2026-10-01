//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/metadata"
)

// An idempotent replay of a finished engine pull is checked against the real
// daemon first (#35). While the daemon holds the image the key replays the
// same job; once the image is removed, the same key runs the pull again and
// the image is back.
func TestL2IdempotentReplayRerunsWhatTheEngineLost(t *testing.T) {
	h := newL2Harness(t)
	seedImage(t, h.remote, "lib/app", "1")
	ref := h.remote + "/lib/app:1"
	h.removeImage(ref)

	ctx := metadata.AppendToOutgoingContext(context.Background(), "idempotency-key", "l2-replay-"+t.Name())
	add := func() *pb.Job {
		t.Helper()
		job, err := h.client.Job().Add(ctx, pullJob(ref, "remote", false))
		if err != nil {
			t.Fatalf("add: %v", err)
		}
		return h.waitDone(job.GetId())
	}

	first := add()
	if first.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q", first.GetState(), first.GetError())
	}
	if again := add(); again.GetId() != first.GetId() {
		t.Fatalf("the image is on the daemon, yet the key ran a new job %s", again.GetId())
	}

	h.removeImage(ref)
	rerun := add()
	if rerun.GetId() == first.GetId() {
		t.Fatal("the image is gone from the daemon, and the key replayed the finished job")
	}
	if rerun.GetState() != pb.JobState_JOB_STATE_DONE || !h.daemonHas(ref) {
		t.Fatalf("the re-run did not put the image back: state=%v error=%q", rerun.GetState(), rerun.GetError())
	}
}
