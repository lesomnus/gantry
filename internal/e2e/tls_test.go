package e2e

import (
	"testing"

	"github.com/lesomnus/gantry/pb"
)

// TLS: gantry copies into a cache served over HTTPS, trusting a private CA via
// the store's ca_cert (insecure off). Proves the ca_cert transport path without
// a real daemon.
func TestTLSCache(t *testing.T) {
	h := newHarness(t, withTLSCache())
	seedImage(t, h.remote, "lib/app", "1")

	job := h.waitDone(h.add(copyReq("remote", "cache")).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("TLS copy state=%v error=%q", job.GetState(), job.GetError())
	}
}

// mTLS: the cache registry REQUIRES a client certificate; the store supplies a
// kind "file" cred (PEM cert/key pair). Proves the file-based client-mTLS
// transport is presented on the copy path.
func TestMTLSCache(t *testing.T) {
	h := newHarness(t, withMTLSCache())
	seedImage(t, h.remote, "lib/app", "1")

	job := h.waitDone(h.add(copyReq("remote", "cache")).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("mTLS copy state=%v error=%q", job.GetState(), job.GetError())
	}
}
