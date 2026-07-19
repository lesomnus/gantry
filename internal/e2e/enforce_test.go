package e2e

import (
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/notaryproject/notation-core-go/testhelper"
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
