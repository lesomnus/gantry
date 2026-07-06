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

// groupByRepo buckets records by their keep-N/max-N grouping key (Record.Repo).
func groupByRepo(recs []Record) map[string][]Record {
	byRepo := map[string][]Record{}
	for _, r := range recs {
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	return byRepo
}

// sortByRecency orders a repo group most-recently-used first, tie-broken by Ref
// so the keep/delete boundary is deterministic.
func sortByRecency(group []Record) {
	sort.Slice(group, func(i, j int) bool {
		ti, tj := group[i].effLastUsed(), group[j].effLastUsed()
		if ti.Equal(tj) {
			return group[i].Ref < group[j].Ref
		}
		return ti.After(tj)
	})
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
//  3. max-N    — of the rest, keep only the MaxN most-recently-used tags per
//     repository; delete the oldest beyond the cap even if not yet stale
//     (deferred during the grace window). in-use/pinned tags do not count.
//  4. keep-N   — the KeepN most-recently-used tags per repository
//  5. age      — of the rest, delete those whose last-used age exceeds MaxAge,
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

	// max-N cap: keep at most MaxN tags per repository, deleting the oldest beyond
	// the cap regardless of age. Deferred during the grace window, since a just-
	// restarted node has no usage history and the "oldest" ordering is unreliable.
	if p.MaxN > 0 {
		inGrace := now.Before(graceUntil)
		over := map[string]bool{}
		for _, group := range groupByRepo(remaining) {
			if len(group) <= p.MaxN {
				continue
			}
			sortByRecency(group)
			if inGrace {
				// Something exceeds the cap but grace holds it: wake at graceUntil.
				if dec.NextAgeOut.IsZero() || graceUntil.Before(dec.NextAgeOut) {
					dec.NextAgeOut = graceUntil
				}
				continue
			}
			for i := p.MaxN; i < len(group); i++ {
				over[group[i].Ref] = true
				dec.Delete = append(dec.Delete, Candidate{
					Ref: group[i].Ref, Digest: group[i].Digest,
					LastUsed: group[i].effLastUsed(), Reason: "max_n_exceeded",
				})
			}
		}
		if len(over) > 0 {
			var rest []Record
			for _, r := range remaining {
				if !over[r.Ref] {
					rest = append(rest, r)
				}
			}
			remaining = rest
		}
	}

	// keep-N most-recently-used tags per repository.
	if p.KeepN > 0 {
		protected := map[string]bool{}
		for _, group := range groupByRepo(remaining) {
			sortByRecency(group)
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
