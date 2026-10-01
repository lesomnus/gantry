//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/lesomnus/gantry/pb"
)

// containerdStore reports whether the daemon keeps images in the containerd
// image store, which is the only one that can hold a digest `as` name.
func (h *l2harness) containerdStore() bool {
	h.t.Helper()
	info, err := h.cli.Info(context.Background())
	if err != nil {
		h.t.Fatalf("docker info: %v", err)
	}
	for _, kv := range info.DriverStatus {
		if kv[0] == "driver-type" && kv[1] == "io.containerd.snapshotter.v1" {
			return true
		}
	}
	return false
}

// A digest `as` name in the repository the daemon pulled from is still on the
// daemon when the job is done (#39).
//
// This is the fleet's shape, and no other test here has it: the nodes pull the
// cache by the registry the release names, so the name the pull creates
// (`repo@sha256:…`) and the name the jobspec uses (`repo:tag@sha256:…`) differ
// only by the tag. To the real daemon those are one reference. Dropping the
// first dropped the second and then the image — a delivery that reported DONE
// and left nothing, which only a real daemon shows: a fake one deletes the one
// name it was asked to.
func TestL2DigestAsInThePullsOwnRepository(t *testing.T) {
	h := newL2Harness(t)
	if !h.containerdStore() {
		t.Skip("the daemon is on the classic image store, which refuses a digest `as` name before pulling")
	}
	d := seedImage(t, h.remote, "lib/app", "1")
	ref := h.remote + "/lib/app@" + d.String()
	as := h.remote + "/lib/app:1@" + d.String()
	h.removeImage(ref)
	t.Cleanup(func() { h.removeImage(ref) })

	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:    ref,
		Source: pb.StoreByName("remote"),
		Target: pb.StoreByName("edge"),
		As:     []string{as},
	}.Build()).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("state=%v error=%q (%s)", job.GetState(), job.GetError(), describe(job))
	}
	if !h.daemonHas(as) {
		t.Fatalf("the job is DONE and the daemon does not hold %s: naming the image dropped it", as)
	}

	// And once more, as a re-applied release does. The image is there now, so
	// this is the delivery that used to be refused outright while a container
	// ran it, and that dropped the image when none did.
	job = h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref:    ref,
		Source: pb.StoreByName("remote"),
		Target: pb.StoreByName("edge"),
		As:     []string{as},
	}.Build()).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("second delivery: state=%v error=%q", job.GetState(), job.GetError())
	}
	if !h.daemonHas(as) {
		t.Fatalf("the second delivery dropped %s", as)
	}
}
