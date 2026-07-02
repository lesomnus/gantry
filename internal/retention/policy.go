package retention

import (
	"path"
	"sort"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// matchPin reports whether a pin (an exact reference or a doublestar pattern)
// protects the record. A pattern is tried against the full ref, the name:tag
// short form (last repo segment), and the bare tag — so "cache.local/a/app:1",
// "*:stable", and "prod-*" all work as plan-gc.md documents.
func matchPin(pin string, r Record) bool {
	if pin == r.Ref {
		return true
	}
	for _, s := range []string{r.Ref, shortName(r), r.Tag} {
		if s == "" {
			continue
		}
		if ok, err := doublestar.Match(pin, s); err == nil && ok {
			return true
		}
	}
	return false
}

func shortName(r Record) string {
	if r.Repo == "" || r.Tag == "" {
		return ""
	}
	return path.Base(r.Repo) + ":" + r.Tag
}

func pinned(pins []string, r Record) bool {
	for _, pin := range pins {
		if matchPin(pin, r) {
			return true
		}
	}
	return false
}

// Evaluate applies the retention policy and returns the delete/keep decision.
// Protection order (a protected image is never deleted):
//  1. in-use   — referenced by a live container (inUse holds refs and digests)
//  2. pinned   — Record.Pinned, or a policy.Pins entry (exact ref or doublestar
//     pattern) matches
//  3. keep-N   — the KeepN most-recently-used tags per repository
//  4. age      — of the rest, delete those whose last-used age exceeds MaxAge,
//     unless still within the grace window (now < graceUntil).
func Evaluate(now time.Time, recs []Record, inUse map[string]bool, p Policy, graceUntil time.Time) Decision {
	var dec Decision
	var remaining []Record
	for _, r := range recs {
		switch {
		case inUse[r.Ref] || (r.Digest != "" && inUse[r.Digest]):
			dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "in_use"})
		case r.Pinned || pinned(p.Pins, r):
			dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "pinned"})
		default:
			remaining = append(remaining, r)
		}
	}

	// keep-N most-recently-used tags per repository.
	if p.KeepN > 0 {
		byRepo := map[string][]Record{}
		for _, r := range remaining {
			byRepo[r.Repo] = append(byRepo[r.Repo], r)
		}
		protected := map[string]bool{}
		for _, group := range byRepo {
			sort.Slice(group, func(i, j int) bool {
				ti, tj := group[i].effLastUsed(), group[j].effLastUsed()
				if ti.Equal(tj) {
					return group[i].Ref < group[j].Ref
				}
				return ti.After(tj) // most recent first
			})
			for i := 0; i < len(group) && i < p.KeepN; i++ {
				protected[group[i].Ref] = true
				dec.Keep = append(dec.Keep, Kept{Ref: group[i].Ref, Reason: "keep_n_recent"})
			}
		}
		var rest []Record
		for _, r := range remaining {
			if !protected[r.Ref] {
				rest = append(rest, r)
			}
		}
		remaining = rest
	}

	// age-based deletion (held off during the grace window).
	for _, r := range remaining {
		if p.MaxAge <= 0 {
			dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "age_gc_disabled"})
			continue
		}
		ageOut := r.effLastUsed().Add(p.MaxAge)
		deletableAt := ageOut
		if graceUntil.After(deletableAt) {
			deletableAt = graceUntil // grace defers deletion
		}
		if !now.Before(deletableAt) { // now >= deletableAt
			dec.Delete = append(dec.Delete, Candidate{
				Ref: r.Ref, Digest: r.Digest, LastUsed: r.effLastUsed(), Reason: "age_exceeded",
			})
			continue
		}
		reason := "within_max_age"
		if !now.Before(ageOut) { // would age out, but grace holds it
			reason = "grace"
		}
		dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: reason})
		if dec.NextAgeOut.IsZero() || deletableAt.Before(dec.NextAgeOut) {
			dec.NextAgeOut = deletableAt
		}
	}
	return dec
}
