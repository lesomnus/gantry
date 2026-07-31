package enforce

import (
	"context"
	"errors"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/verify"
)

// act is the outcome of a decision.
type act int

const (
	actAllow act = iota
	actKill
)

type decision struct {
	action act
	reason string
	digest string // the content digest the decision was keyed on, for logging
}

func allow(reason string) decision { return decision{action: actAllow, reason: reason} }
func kill(reason string) decision  { return decision{action: actKill, reason: reason} }

// decide resolves the verdict for a started container. The verdict is keyed on
// the image's TOP-LEVEL content digest (from RepoDigests — a signature is over
// the index, not the platform-specific manifest), consulted in precedence:
//
//  1. a fresh cached verdict (trusted -> allow, untrusted -> kill), offline;
//  2. a live verification (which also consults the local layout and populates
//     the cache): trusted -> allow, unsigned/untrusted -> kill;
//  3. otherwise (no digest, no matching source store, or the registry is
//     unreachable) the on_unavailable policy, with grace honoring an
//     expired-but-trusted cached verdict.
func (m *Manager) decide(ctx context.Context, eng Engine, ev down.StartEvent) decision {
	ci, err := eng.ResolveImage(ctx, ev.ContainerID)
	if err != nil {
		// The container may already be gone, or the daemon is unhappy; there is
		// nothing to key a kill on. Never kill on an inspect failure.
		return allow("image resolve failed: " + err.Error())
	}
	digest := topLevelDigest(ci)
	if digest == "" {
		// No registry provenance (locally built, docker load, or a bare image id).
		return m.onUnavailable("no content digest (no registry provenance)", "")
	}

	// 1. Fresh cached verdict — decisive and offline.
	if v, found, _ := m.cache.Get(digest); found && !v.Expired(m.now()) {
		if v.Trusted {
			return decision{action: actAllow, reason: "cache: trusted", digest: digest}
		}
		return decision{action: actKill, reason: "cache: untrusted", digest: digest}
	}

	// 2. Live verification against the source registry that the image was pulled
	//    from (matched by the RepoDigest host). This also consults the local
	//    signature layout and writes the verdict back to the cache.
	//    There may be more than one: an image gantry routed through a cache is
	//    recorded under the CACHE's host, so the store the host matches is asked
	//    first and the origin that declares it as a cache is asked after. A refusal
	//    is therefore only acted on once every store that could hold the signature
	//    has been asked — the signature is over the digest, so proof from any of
	//    them is proof about the same bytes.
	var refuse error
	for _, c := range m.sourcesFor(ci) {
		res, verr := m.verifier.Verify(ctx, c.store, c.ref)
		switch {
		case verr == nil && res.Verified():
			return decision{action: actAllow, reason: "verified", digest: digest}
		case errors.Is(verr, verify.ErrUnsigned) || errors.Is(verr, verify.ErrUntrusted):
			if refuse == nil {
				refuse = verr
			}
		default:
			// verr is nil-without-digest (verify-if-present unsigned-allowed) or a
			// transient error (unreachable/timeout/not found): fall to the policy.
		}
	}
	if refuse != nil {
		return decision{action: actKill, reason: "verification: " + refuse.Error(), digest: digest}
	}

	// 3. No usable live answer.
	return m.onUnavailable("verification unavailable", digest)
}

// onUnavailable applies the on_unavailable policy. Under grace it honors an
// expired-but-known trusted verdict before falling back to allow-and-log.
func (m *Manager) onUnavailable(reason, digest string) decision {
	switch m.policy {
	case "kill":
		return decision{action: actKill, reason: "on_unavailable=kill: " + reason, digest: digest}
	case "allow":
		return decision{action: actAllow, reason: "on_unavailable=allow: " + reason, digest: digest}
	default: // grace
		if digest != "" {
			if v, found, _ := m.cache.Get(digest); found && v.Trusted {
				return decision{action: actAllow, reason: "grace: honoring expired trusted verdict", digest: digest}
			}
		}
		return decision{action: actAllow, reason: "grace: no verdict, allowing (" + reason + ")", digest: digest}
	}
}

// topLevelDigest returns the image's top-level content digest from its
// RepoDigests — the digest a notation signature is over. It deliberately does
// NOT fall back to the platform-specific ManifestDigest, which is unsigned and
// would make every multi-arch image look untrusted.
func topLevelDigest(ci down.ContainerImage) string {
	for _, rd := range ci.RepoDigests {
		if i := strings.LastIndex(rd, "@"); i >= 0 {
			return rd[i+1:]
		}
	}
	return ""
}

// sourceFor builds the (digest reference, source store) to live-verify against,
// by matching a RepoDigest's registry host to a configured OCI store — so the
// verify uses that store's transport/insecure/credentials (the signature lives
// in the cache registry the daemon pulled from). Returns ok=false when no
// RepoDigest names a configured store.
// verifySource is one place a running image's signature may live.
type verifySource struct {
	ref   name.Digest
	store config.StoreConfig
}

// sourcesFor lists the stores that could hold the signature for a running image,
// in the order they should be asked.
//
// The first is the store whose host the daemon recorded — which for an image
// gantry routed through a cache is the CACHE, not the registry the operator
// pointed the job at. That store is asked first because it is what actually
// served the node. The origins that declare it as a cache follow: a cache is only
// ever a copy of their content, its referrers can be collected independently of
// the image, and the signature is over the digest — so asking the origin about
// the same digest asks about the same bytes. Without this, an image whose cache
// copy lost (or never received) its signature is killed even though the registry
// it came from can prove it.
func (m *Manager) sourcesFor(ci down.ContainerImage) []verifySource {
	ref, from, ok := m.sourceFor(ci)
	if !ok {
		return nil
	}
	out := []verifySource{{ref: ref, store: from}}
	for _, origin := range m.cachedBy[from.Name] {
		alt, err := name.NewDigest(origin.Host+"/"+ref.Context().RepositoryStr()+"@"+ref.DigestStr(), name.Insecure)
		if err != nil {
			continue
		}
		// Enforcement is a run-time requirement, independent of the store's own
		// admission mode — same reasoning as sourceFor's.
		origin.Verify = &config.StoreVerify{Mode: config.VerifyRequire}
		out = append(out, verifySource{ref: alt, store: origin})
	}
	return out
}

func (m *Manager) sourceFor(ci down.ContainerImage) (name.Digest, config.StoreConfig, bool) {
	for _, rd := range ci.RepoDigests {
		ref, err := name.NewDigest(rd, name.Insecure)
		if err != nil {
			continue
		}
		if st, ok := m.ociByHost[ref.Context().RegistryStr()]; ok {
			// Enforcement is a run-time signature REQUIREMENT, independent of the
			// store's admission verify mode. Force `require` for the enforcement
			// verify so an unsigned image is classified (and quarantined) even when
			// admission is off or verify-if-present for this store — otherwise the
			// verifier would short-circuit on an off effective mode and enforcement
			// would silently degrade to on_unavailable for every image. st is a copy
			// (map range value), so this does not mutate the shared config.
			st.Verify = &config.StoreVerify{Mode: config.VerifyRequire}
			return ref, st, true
		}
	}
	return name.Digest{}, config.StoreConfig{}, false
}
