package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/pb"
	"github.com/notaryproject/notation-core-go/testhelper"
	"google.golang.org/protobuf/proto"
)

// Feature: runtime signature enforcement (quarantine). With enforcement enabled
// on the `edge` engine, a started container whose image is signed by a trusted
// Root CA is left running while one whose image is unsigned is force-removed.
//
// Enforcement uses require semantics INDEPENDENT of the admission verify mode, so
// it quarantines an unsigned container whether admission verification is `require`
// or `off` — this drives the same in-process production wiring (app.Build) the
// other features' L1 tests do.
func TestEnforcement(t *testing.T) {
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()

	run := func(t *testing.T, mode config.VerifyMode) {
		trust := writeTrustStore(t, root.Cert)
		h := newHarness(t,
			withVerify(config.VerifyConfig{
				Mode: mode, Provider: "notation", TrustStore: trust,
				Level: "permissive", Timeout: config.Duration(20 * time.Second),
			}),
			withEnforce(),
		)

		signedDg := seedImage(t, h.remote, "lib/signed", "1")
		signRef(t, h.remote+"/lib/signed:1", root, leaf)
		unsignedDg := seedImage(t, h.remote, "lib/unsigned", "1")

		h.engine.startContainer("c-signed", h.remote+"/lib/signed:1", h.remote+"/lib/signed@"+signedDg.String())
		h.engine.startContainer("c-unsigned", h.remote+"/lib/unsigned:1", h.remote+"/lib/unsigned@"+unsignedDg.String())

		// Events are processed in order on one watcher, so once the unsigned
		// container (started second) is removed, the signed one has been decided.
		if !eventually(5*time.Second, func() bool { return h.engine.wasRemoved("c-unsigned") }) {
			t.Fatal("unsigned container was not quarantined")
		}
		if h.engine.wasRemoved("c-signed") {
			t.Error("signed container was wrongly quarantined")
		}
	}

	t.Run("admission require", func(t *testing.T) { run(t, config.VerifyRequire) })
	t.Run("admission off", func(t *testing.T) { run(t, config.VerifyOff) })
}

// eventually polls cond until it is true or the deadline passes.
func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestEnforcementMultiArch covers the multi-arch pipeline the single-arch tests
// don't: a signed multi-platform image is signed at the INDEX, copy_referrers
// carries that index signature into the cache, and a platform-narrowed container
// (whose RepoDigest is the index digest, the same digest the daemon anchors an
// anchored platform pull to) verifies against the index signature and is allowed,
// while an unsigned multi-arch image is quarantined.
func TestEnforcementMultiArch(t *testing.T) {
	root := testhelper.GetRSARootCertificate()
	leaf := testhelper.GetRSALeafCertificate()
	trust := writeTrustStore(t, root.Cert)

	h := newHarness(t,
		withVerify(config.VerifyConfig{
			Mode: config.VerifyRequire, Provider: "notation", TrustStore: trust,
			Level: "permissive", Timeout: config.Duration(20 * time.Second),
		}),
		withEnforce(),
	)

	// Signed multi-arch index in `remote`; signRef signs the ref, which resolves
	// to the INDEX — so the signature is a referrer of the index digest.
	idxDigest := seedPlatformIndex(t, h.remote, "lib/multi", "1", "linux/amd64", "linux/arm64")
	signRef(t, h.remote+"/lib/multi:1", root, leaf)

	// copy_referrers carries the index signature into the cache (a full, verbatim
	// all-platforms copy — copy_referrers refuses platform narrowing).
	job := h.waitDone(h.add(pb.JobAddRequest_builder{
		Ref: "lib/multi:1", Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache"),
		CopyReferrers: proto.Bool(true),
	}.Build()).GetId())
	if job.GetState() != pb.JobState_JOB_STATE_DONE {
		t.Fatalf("multi-arch copy state=%v error=%q", job.GetState(), job.GetError())
	}
	// The cache copy re-verifies against the travelled index signature, pinned to
	// the index digest.
	vc := pb.NewVerifyServiceClient(h.conn)
	rep, err := vc.Check(context.Background(), pb.VerifyCheckRequest_builder{
		Ref: proto.String("lib/multi:1"), Source: pb.StoreByName("cache"),
	}.Build())
	if err != nil || !rep.GetVerified() {
		t.Fatalf("index signature did not travel into the cache: verified=%v err=%v", rep.GetVerified(), err)
	}
	if rep.GetDigest() != idxDigest.String() {
		t.Errorf("cache verify pinned %s, want the index digest %s", rep.GetDigest(), idxDigest)
	}

	// A platform-narrowed container records the INDEX digest as its RepoDigest;
	// enforcement verifies it against the cache's index signature -> allowed.
	h.engine.startContainer("c-amd64", h.cache+"/lib/multi:1", h.cache+"/lib/multi@"+idxDigest.String())

	// An unsigned multi-arch image (seeded straight into the cache) is quarantined.
	unsignedIdx := seedPlatformIndex(t, h.cache, "lib/nosig", "1", "linux/amd64", "linux/arm64")
	h.engine.startContainer("c-unsigned", h.cache+"/lib/nosig:1", h.cache+"/lib/nosig@"+unsignedIdx.String())

	if !eventually(5*time.Second, func() bool { return h.engine.wasRemoved("c-unsigned") }) {
		t.Fatal("unsigned multi-arch container was not quarantined")
	}
	if h.engine.wasRemoved("c-amd64") {
		t.Error("a platform-narrowed container of a signed multi-arch image was wrongly quarantined")
	}
}
