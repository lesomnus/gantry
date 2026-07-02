// Package verify checks source-image signatures at job admission. When enabled,
// gantry resolves the source reference on its origin registry, verifies its
// signature per policy, and — on success — returns the verified digest so the
// caller can pin exactly what was verified. A failure rejects the job.
//
// The first (and only, for now) provider is Notary Project / notation: OCI-native
// signatures stored as referrer artifacts, verified against a trust store (CA
// certs) and a trust policy.
package verify

import (
	"context"
	"errors"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
)

// ErrUnsigned is returned when a source image has no signature and the effective
// mode is "require".
var ErrUnsigned = errors.New("image is not signed")

// ErrUntrusted is returned when a signature is present but fails verification.
var ErrUntrusted = errors.New("image signature verification failed")

// ErrNotFound is returned when the reference does not exist on the source.
var ErrNotFound = errors.New("reference not found")

// ErrBadTrustMaterial marks trust store / policy VALIDATION failures — the
// operator's material is wrong — as opposed to transient I/O reading it.
var ErrBadTrustMaterial = errors.New("invalid trust material")

// Result reports a completed verification: the mode that was in effect and the
// verified digest to pin. A zero Digest means nothing was verified (mode off,
// or verify-if-present with no signature).
type Result struct {
	Mode   config.VerifyMode
	Digest v1.Hash
}

// Verified reports whether a signature was actually verified.
func (r Result) Verified() bool { return r.Digest.Hex != "" }

// Verifier checks a source image's signature per the effective policy.
type Verifier interface {
	// Verify checks src on registry store `from` per the effective mode. It
	// returns the effective mode and the verified digest to pin. It returns
	// ErrUnsigned or ErrUntrusted (wrapped) when the image is rejected, or a
	// non-sentinel error when verification could not be completed (unreachable
	// registry, etc.); every non-nil error must reject the job (fail-closed).
	Verify(ctx context.Context, from config.StoreConfig, src name.Reference) (Result, error)
}

// Service is the verification surface the API serves: checking references,
// introspecting the trust configuration, and hot-reloading it.
type Service interface {
	Verifier
	// Describe reports the effective trust configuration (never key material).
	Describe() Description
	// Reload rebuilds the verifier from config + disk, swapping it in only on
	// success; the previous verifier stays active on failure.
	Reload() (Description, error)
}
