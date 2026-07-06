// Package retention maintains a persisted per-image last-used index (the signal
// neither docker nor containerd exposes natively) and evaluates the GC policy:
// protect in-use, protect pinned, keep the N most-recently-used tags per repo,
// then delete images whose last-used age exceeds max_age.
package retention

import "time"

// Record is the per-(engine, image-ref) retention state. The bbolt key is Ref.
type Record struct {
	Ref             string    `json:"ref"`              // full local ref incl. tag, e.g. "cache.local/team/app:1.2"
	Repo            string    `json:"repo"`             // RepositoryStr(); the keep-N grouping key
	Tag             string    `json:"tag"`              // tag portion; "" for digest refs
	Digest          string    `json:"digest,omitempty"` // resolved manifest digest, if known
	LastUsed        time.Time `json:"last_used"`        // the last-USED signal; zero => never observed used
	LastDistributed time.Time `json:"last_distributed,omitempty"`
	FirstSeen       time.Time `json:"first_seen"`
	Pinned          bool      `json:"pinned,omitempty"` // explicit pin on this exact ref
}

// effLastUsed is the timestamp used for age decisions: real last-used, else the
// distribute time, else first-seen.
func (r Record) effLastUsed() time.Time {
	switch {
	case !r.LastUsed.IsZero():
		return r.LastUsed
	case !r.LastDistributed.IsZero():
		return r.LastDistributed
	default:
		return r.FirstSeen
	}
}

// Policy is the retention policy applied at GC time.
type Policy struct {
	MaxAge time.Duration `json:"max_age"`
	KeepN  int           `json:"keep_n"`
	// MaxN caps the number of tags kept per repository: when a repo has more than
	// MaxN non-protected tags, the oldest beyond the cap are deleted even if not
	// yet past MaxAge. Zero disables the cap. When both are set MaxN must be >=
	// KeepN. The cap is deferred during the startup grace window, like age GC.
	MaxN int      `json:"max_n"`
	Pins []string `json:"pins"` // exact refs
}

// Candidate is one image marked for deletion.
type Candidate struct {
	Ref      string    `json:"ref"`
	Digest   string    `json:"digest,omitempty"`
	LastUsed time.Time `json:"last_used"`
	Reason   string    `json:"reason"` // age_exceeded | max_n_exceeded
}

// Kept is one image protected from deletion, with the protecting reason.
type Kept struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"` // in_use | pinned | keep_n_recent | within_max_age | grace | age_gc_disabled
}

// Decision is the result of evaluating the policy over a store's records.
type Decision struct {
	Delete []Candidate `json:"delete"`
	Keep   []Kept      `json:"keep"`

	// NextAgeOut is the soonest a currently-kept record could become
	// age-deletable; the scheduler waits until then (or a usage event). Zero
	// means nothing is on an age path.
	NextAgeOut time.Time `json:"next_age_out,omitzero"`
}
