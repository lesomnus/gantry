package verify

import (
	"encoding/json"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	bolt "go.etcd.io/bbolt"
)

// bktVerdict is the single flat bucket: verdict/<content-digest> -> json(Verdict).
// A content digest is globally unique, so — unlike the retention index — there is
// no per-engine nesting.
var bktVerdict = []byte("verdict")

// Verdict is a cached verification decision for one content digest.
type Verdict struct {
	// Digest is the content digest this verdict is for ("sha256:...").
	Digest string `json:"digest"`
	// Trusted is true when the image is signed by a trusted Root CA, false when it
	// is definitively unsigned or untrusted. A "could not reach the registry"
	// outcome is never stored — it is not a verdict.
	Trusted bool `json:"trusted"`
	// Mode is the effective verify mode when the verdict was produced.
	Mode config.VerifyMode `json:"mode,omitempty"`
	// SourceRef is the registry reference the signature was verified against, so
	// the refresh sweeper can re-verify it.
	SourceRef  string    `json:"source_ref,omitempty"`
	VerifiedAt time.Time `json:"verified_at"`
	// RefreshAfter is the soft revalidation age: past it the sweeper re-verifies,
	// but the verdict stays usable until ExpiresAt.
	RefreshAfter time.Time `json:"refresh_after"`
	// ExpiresAt is the hard TTL: past it the verdict is unusable and the image
	// must be re-verified (enforcement may still honor it under the grace policy).
	ExpiresAt time.Time `json:"expires_at"`
}

// Expired reports whether the verdict is past its hard TTL as of now.
func (v Verdict) Expired(now time.Time) bool { return now.After(v.ExpiresAt) }

// StaleForRefresh reports whether the verdict is past its soft refresh age.
func (v Verdict) StaleForRefresh(now time.Time) bool { return now.After(v.RefreshAfter) }

// Cache is the durable verdict store (bbolt), keyed by content digest. It is
// shared by admission verification (which writes verdicts as a side effect) and
// runtime enforcement (which reads them, offline). Safe for concurrent use.
type Cache struct {
	db      *bolt.DB
	ttl     time.Duration
	refresh time.Duration
	now     func() time.Time
}

// CacheOption customizes a Cache at open time.
type CacheOption func(*Cache)

// WithNow overrides the clock, for deterministic tests.
func WithNow(fn func() time.Time) CacheOption { return func(c *Cache) { c.now = fn } }

// OpenCache opens (creating if needed) the bbolt verdict cache at path. ttl is
// the hard verdict max-age; refresh is the soft revalidation age (refresh <= ttl,
// enforced by config validation). The file takes an exclusive lock.
func OpenCache(path string, ttl, refresh time.Duration, opts ...CacheOption) (*Cache, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bktVerdict)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	c := &Cache{db: db, ttl: ttl, refresh: refresh, now: time.Now}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func (c *Cache) Close() error { return c.db.Close() }

// TTL and Refresh expose the configured durations (the refresh sweeper uses them
// to schedule its next wake).
func (c *Cache) TTL() time.Duration     { return c.ttl }
func (c *Cache) Refresh() time.Duration { return c.refresh }

// Get returns the cached verdict for a digest and whether one exists. It does NOT
// filter by expiry: callers apply the TTL/refresh policy themselves (enforcement
// honors an expired-but-trusted verdict under the grace policy).
func (c *Cache) Get(digest string) (Verdict, bool, error) {
	var (
		v     Verdict
		found bool
	)
	err := c.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bktVerdict).Get([]byte(digest))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &v)
	})
	return v, found, err
}

// Put records a verdict for a digest, stamping VerifiedAt = now and deriving
// RefreshAfter / ExpiresAt from the cache's refresh / ttl. A later Put for the
// same digest overwrites (re-stamping the timestamps) — this is how the refresh
// sweeper renews a still-valid verdict.
func (c *Cache) Put(digest string, trusted bool, mode config.VerifyMode, sourceRef string) error {
	now := c.now()
	v := Verdict{
		Digest:       digest,
		Trusted:      trusted,
		Mode:         mode,
		SourceRef:    sourceRef,
		VerifiedAt:   now,
		RefreshAfter: now.Add(c.refresh),
		ExpiresAt:    now.Add(c.ttl),
	}
	enc, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktVerdict).Put([]byte(digest), enc)
	})
}

// ForEach iterates every stored verdict (used by the refresh sweeper). The
// callback must not retain the Verdict beyond the call; a callback error aborts
// iteration and is returned.
func (c *Cache) ForEach(fn func(Verdict) error) error {
	return c.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bktVerdict).ForEach(func(_, raw []byte) error {
			var v Verdict
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			return fn(v)
		})
	})
}

// Count returns the number of stored verdicts.
func (c *Cache) Count() (int, error) {
	var n int
	err := c.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket(bktVerdict).Stats().KeyN
		return nil
	})
	return n, err
}

// Delete removes a verdict (e.g. on trust-material rotation); a missing key is
// not an error.
func (c *Cache) Delete(digest string) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktVerdict).Delete([]byte(digest))
	})
}
