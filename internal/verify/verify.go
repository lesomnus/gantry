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

// Verifier checks a source image's signature per the effective policy.
type Verifier interface {
	// Verify checks src on registry store `from` per the effective mode. It
	// returns the verified digest to pin (a zero Hash when nothing was verified —
	// mode off, or verify-if-present with no signature). It returns ErrUnsigned or
	// ErrUntrusted (wrapped) when the image is rejected, or a non-sentinel error
	// when verification could not be completed (unreachable registry, etc.); every
	// non-nil error must reject the job (fail-closed).
	Verify(ctx context.Context, from config.StoreConfig, src name.Reference) (v1.Hash, error)
}
