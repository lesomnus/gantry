package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
)

// Description reports the effective verification configuration — never key
// material. Anchor expiries drive alerting: in require mode an expired CA
// fails every job.
type Description struct {
	Enabled  bool         `json:"enabled"`
	Provider string       `json:"provider,omitempty"`
	Mode     string       `json:"mode"` // global default: off | verify-if-present | require
	Level    string       `json:"level,omitempty"`
	Policies []PolicyInfo `json:"policies,omitempty"`
	Anchors  []AnchorInfo `json:"anchors,omitempty"`
}

// PolicyInfo is one trust-policy statement.
type PolicyInfo struct {
	Name              string   `json:"name"`
	RegistryScopes    []string `json:"registry_scopes"`
	TrustedIdentities []string `json:"trusted_identities"`
	Level             string   `json:"verification_level"`
}

// AnchorInfo identifies one trust-anchor certificate.
type AnchorInfo struct {
	Subject     string    `json:"subject"`
	Fingerprint string    `json:"fingerprint"` // sha256 over the DER encoding
	NotAfter    time.Time `json:"not_after"`
}

func (n *notaryVerifier) describe() Description {
	d := Description{
		Enabled:  true,
		Provider: "notation",
		Mode:     string(n.cfg.Mode),
		Level:    n.cfg.Level,
	}
	if d.Mode == "" {
		d.Mode = string(config.VerifyOff)
	}
	for _, p := range n.policy.TrustPolicies {
		d.Policies = append(d.Policies, PolicyInfo{
			Name:              p.Name,
			RegistryScopes:    p.RegistryScopes,
			TrustedIdentities: p.TrustedIdentities,
			Level:             p.SignatureVerification.VerificationLevel,
		})
	}
	for _, c := range n.certs {
		sum := sha256.Sum256(c.Raw)
		d.Anchors = append(d.Anchors, AnchorInfo{
			Subject:     c.Subject.String(),
			Fingerprint: hex.EncodeToString(sum[:]),
			NotAfter:    c.NotAfter,
		})
	}
	return d
}

// Swappable is a hot-reloadable Verifier: Verify delegates to the current
// notation verifier, and Reload rebuilds it from config + disk, swapping only
// on success — CA rotation without restarting gantry (and without killing
// in-flight jobs or the job history).
type Swappable struct {
	cfg config.VerifyConfig
	cur atomic.Pointer[notaryVerifier]

	// reload serializes Reload's read-build-store sequence: two concurrent
	// reloads racing a disk rotation could otherwise leave the OLDER read
	// active after both returned success. Verify/Describe stay lock-free.
	reload sync.Mutex
}

var _ Service = (*Swappable)(nil)

// NewSwappable builds the initial verifier fail-fast, like New.
func NewSwappable(cfg config.VerifyConfig) (*Swappable, error) {
	n, err := newNotary(cfg)
	if err != nil {
		return nil, err
	}
	s := &Swappable{cfg: cfg}
	s.cur.Store(n)
	return s, nil
}

func (s *Swappable) Verify(ctx context.Context, from config.StoreConfig, src name.Reference) (Result, error) {
	return s.cur.Load().Verify(ctx, from, src)
}

func (s *Swappable) Describe() Description { return s.cur.Load().describe() }

// Reload re-reads the trust store and policy from disk with the same fail-fast
// validation as startup. In-flight verifications finish on the old verifier.
func (s *Swappable) Reload() (Description, error) {
	s.reload.Lock()
	defer s.reload.Unlock()
	n, err := newNotary(s.cfg)
	if err != nil {
		return Description{}, err
	}
	s.cur.Store(n)
	return n.describe(), nil
}
