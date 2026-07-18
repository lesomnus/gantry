package verify

import (
	"context"
	"errors"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
)

// Caching decorates a verify.Service with the durable verdict cache. It serves
// two purposes:
//
//   - Populate the cache: every live verification with a definitive outcome
//     (trusted, or unsigned/untrusted for a digest reference) is written back,
//     keyed by the verified content digest. Runtime enforcement later reads these
//     verdicts offline. A "could not reach the registry" outcome is NEVER cached
//     — per the verify.Verifier contract it is a non-sentinel error, and caching
//     it would let a transient outage masquerade as a verdict.
//
//   - Accelerate: a digest reference with a fresh (within hard TTL) trusted
//     verdict is answered from the cache without touching the registry.
//
// The decorator's Verify stays fail-closed (it surfaces the wrapped service's
// error). The grace-on-outage policy lives in the enforcement read path, which
// reads the cache directly — not here — so the admission and RPC surfaces keep
// their existing semantics.
type Caching struct {
	Service
	cfg   config.VerifyConfig
	cache *Cache
	now   func() time.Time
}

var _ Service = (*Caching)(nil)

// CachingOption customizes a Caching decorator.
type CachingOption func(*Caching)

// CachingWithNow overrides the clock, for deterministic tests.
func CachingWithNow(fn func() time.Time) CachingOption { return func(c *Caching) { c.now = fn } }

// NewCaching wraps inner with cache. cfg is used to resolve the effective mode
// per source store (the same rule the underlying verifier applies), so the
// decorator never serves a cached verdict when verification is off for a store.
func NewCaching(inner Service, cache *Cache, cfg config.VerifyConfig, opts ...CachingOption) *Caching {
	c := &Caching{Service: inner, cfg: cfg, cache: cache, now: time.Now}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Caching) Verify(ctx context.Context, from config.StoreConfig, src name.Reference) (Result, error) {
	mode := c.cfg.EffectiveMode(from)
	if mode == config.VerifyOff {
		// Verification is off for this store: match the wrapped verifier exactly
		// (no digest pinned) and touch neither the cache nor the registry.
		return Result{Mode: mode}, nil
	}

	// Offline fast path: a digest reference names immutable content, so a fresh
	// trusted verdict can be served without a registry round-trip. Tag references
	// are never served from the cache — a tag can be repointed to new content.
	if dg, ok := src.(name.Digest); ok {
		if v, found, err := c.cache.Get(dg.DigestStr()); err == nil && found && v.Trusted && !v.Expired(c.now()) {
			if h, herr := v1.NewHash(dg.DigestStr()); herr == nil {
				return Result{Mode: mode, Digest: h}, nil
			}
		}
	}

	res, err := c.Service.Verify(ctx, from, src)
	c.record(src, res, err)
	return res, err
}

// record writes a definitive verdict back to the cache. It caches nothing for a
// transient failure (unreachable registry, timeout, ErrNotFound) or an
// unsigned-but-allowed result, so only real trust decisions are persisted.
func (c *Caching) record(src name.Reference, res Result, err error) {
	switch {
	case err == nil && res.Verified():
		// Trusted. Key by the VERIFIED digest, so a tag verify still populates a
		// digest-keyed verdict that the digest-keyed reader (enforcement) hits.
		_ = c.cache.Put(res.Digest.String(), true, res.Mode, src.Name())
	case errors.Is(err, ErrUnsigned) || errors.Is(err, ErrUntrusted):
		// Definitively untrusted — but only cacheable when the key is known. A
		// digest source gives it directly; a tag source does not (the resolved
		// digest is not carried on the error path), so a tag reject is not cached.
		if dg, ok := src.(name.Digest); ok {
			_ = c.cache.Put(dg.DigestStr(), false, res.Mode, src.Name())
		}
	default:
		// err == nil with no digest (verify-if-present, unsigned-allowed), or a
		// non-sentinel/transient error: never cached.
	}
}
