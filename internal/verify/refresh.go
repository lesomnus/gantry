package verify

import (
	"context"
	"errors"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
)

// Refresher keeps trusted verdicts current within their soft-refresh window: it
// periodically re-verifies entries past RefreshAfter and re-stamps them, so a CA
// revocation (or a removed signature) is reflected before the hard TTL. It uses
// the RAW verifier — NOT the caching decorator — so a re-check actually reaches
// the registry / local layout instead of being served from the very cache it is
// refreshing. On an unreachable registry it leaves the entry untouched, so the
// grace policy keeps honoring the still-unexpired verdict.
type Refresher struct {
	cache    *Cache
	verifier Verifier
	// resolve maps a stored SourceRef to the (store, reference) to re-verify.
	resolve func(sourceRef string) (config.StoreConfig, name.Reference, bool)
	now     func() time.Time
}

// RefresherOption customizes a Refresher.
type RefresherOption func(*Refresher)

// RefresherWithNow overrides the clock (tests).
func RefresherWithNow(fn func() time.Time) RefresherOption {
	return func(r *Refresher) { r.now = fn }
}

// NewRefresher builds a Refresher. verifier MUST be the raw verifier (e.g. the
// *Swappable), not a Caching decorator.
func NewRefresher(cache *Cache, verifier Verifier, resolve func(string) (config.StoreConfig, name.Reference, bool), opts ...RefresherOption) *Refresher {
	r := &Refresher{cache: cache, verifier: verifier, resolve: resolve, now: time.Now}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Run sweeps until ctx is cancelled. It ticks at the soft-refresh interval,
// capped at an hour so a multi-week refresh window is still scanned regularly
// (the per-entry RefreshAfter gates whether an entry is actually re-verified).
func (r *Refresher) Run(ctx context.Context) {
	interval := r.cache.Refresh()
	if interval <= 0 {
		return
	}
	if interval > time.Hour {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep re-verifies every stale trusted verdict once. Exported for tests.
func (r *Refresher) Sweep(ctx context.Context) {
	now := r.now()
	var stale []Verdict
	_ = r.cache.ForEach(func(v Verdict) error {
		// Refresh trusted AND untrusted stale entries: a trusted verdict may have
		// been revoked, and an untrusted (unsigned) one may have since been signed.
		// Only definitive re-checks change a verdict; a transient error leaves it.
		if v.StaleForRefresh(now) && v.SourceRef != "" {
			stale = append(stale, v)
		}
		return nil
	})
	for _, v := range stale {
		from, src, ok := r.resolvable(v.SourceRef)
		if !ok {
			continue
		}
		res, err := r.verifier.Verify(ctx, from, src)
		switch {
		case err == nil && res.Verified():
			_ = r.cache.Put(v.Digest, true, res.Mode, v.SourceRef) // renew
		case errors.Is(err, ErrUnsigned) || errors.Is(err, ErrUntrusted):
			_ = r.cache.Put(v.Digest, false, res.Mode, v.SourceRef) // revoked -> flip
		default:
			// unreachable / transient: leave the entry so grace keeps honoring it.
		}
	}
}

// resolvable resolves a SourceRef to (store, reference) and only accepts a DIGEST
// reference — re-verifying a tag could drift to different content if the tag
// moved, which must not renew the old digest's verdict.
func (r *Refresher) resolvable(sourceRef string) (config.StoreConfig, name.Reference, bool) {
	from, ref, ok := r.resolve(sourceRef)
	if !ok {
		return config.StoreConfig{}, nil, false
	}
	if _, isDigest := ref.(name.Digest); !isDigest {
		return config.StoreConfig{}, nil, false
	}
	return from, ref, true
}
