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

// groupByRepo buckets records by their per-repo grouping key (Record.Repo).
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

// resolvePolicy resolves the retention policy for a repository from a store's
// rules. Among rules whose pattern matches the repo, each scalar field takes the
// value from the most specific matching rule that sets it, and pins are the union
// of every matching rule ("cascade"). Specificity: longest literal prefix wins,
// then most literal characters, then lexicographic order for determinism.
// managed is false when no rule matches — the repo is then left untouched.
func resolvePolicy(repo string, rules []Rule) (p Policy, managed bool) {
	var matched []Rule
	for _, r := range rules {
		if ok, err := doublestar.Match(r.Repo, repo); err == nil && ok {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return Policy{}, false
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return moreSpecific(matched[i].Repo, matched[j].Repo)
	})

	var age *time.Duration
	var keepN, maxN *int
	for _, r := range matched { // most specific first; first setter of each field wins
		if age == nil {
			age = r.MaxAge
		}
		if keepN == nil {
			keepN = r.KeepN
		}
		if maxN == nil {
			maxN = r.MaxN
		}
		p.Pins = append(p.Pins, r.Pins...)
	}
	if age != nil {
		p.MaxAge = *age
	}
	if keepN != nil {
		p.KeepN = *keepN
	}
	if maxN != nil {
		p.MaxN = *maxN
	}
	return p, true
}

// moreSpecific reports whether pattern a is more specific than b: longer literal
// prefix (leading characters before the first wildcard) wins; ties are broken by
// more literal characters overall, then lexicographically.
func moreSpecific(a, b string) bool {
	if pa, pb := literalPrefixLen(a), literalPrefixLen(b); pa != pb {
		return pa > pb
	}
	if la, lb := literalCount(a), literalCount(b); la != lb {
		return la > lb
	}
	return a < b
}

// literalPrefixLen is the number of leading bytes before the first wildcard
// metacharacter (*, ?, [, {).
func literalPrefixLen(pattern string) int {
	for i, c := range pattern {
		if isWildcardMeta(c) {
			return i
		}
	}
	return len(pattern)
}

// literalCount is the number of non-wildcard-metacharacter runes in the pattern.
func literalCount(pattern string) int {
	n := 0
	for _, c := range pattern {
		if !isWildcardMeta(c) {
			n++
		}
	}
	return n
}

func isWildcardMeta(c rune) bool {
	switch c {
	case '*', '?', '[', ']', '{', '}':
		return true
	}
	return false
}

// Evaluate applies a store's per-repo retention rules and returns the delete/keep
// decision. Records are grouped by repository; for each repo the matching rules
// are resolved into one Policy (see resolvePolicy) and applied. A repo that
// matches no rule is left unmanaged — kept, never deleted.
//
// Within a managed repo the protection order is:
//  1. in-use   — referenced by a live container (inUse holds refs and digests)
//  2. pinned   — Record.Pinned, or a resolved pin pattern matches
//  3. max-N    — keep only the MaxN most-recently-used tags; delete the oldest
//     beyond the cap even if not yet stale (deferred during the grace window)
//  4. keep-N   — the KeepN most-recently-used tags
//  5. age      — delete those whose last-used age exceeds MaxAge (deferred during
//     the grace window)
func Evaluate(now time.Time, recs []Record, inUse map[string]bool, rules []Rule, graceUntil time.Time) Decision {
	var dec Decision
	for repo, group := range groupByRepo(recs) {
		p, managed := resolvePolicy(repo, rules)
		if !managed {
			for _, r := range group {
				dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "unmanaged"})
			}
			continue
		}
		evalGroup(now, group, inUse, p, graceUntil, &dec)
	}
	return dec
}

// evalGroup evaluates one repository's records against its resolved policy,
// appending to dec.
func evalGroup(now time.Time, group []Record, inUse map[string]bool, p Policy, graceUntil time.Time, dec *Decision) {
	var remaining []Record
	for _, r := range group {
		switch {
		case inUse[r.Ref] || (r.Digest != "" && inUse[r.Digest]):
			dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "in_use"})
		case r.Pinned || pinned(p.Pins, r):
			dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "pinned"})
		default:
			remaining = append(remaining, r)
		}
	}

	// max-N cap: keep at most MaxN tags, deleting the oldest beyond the cap
	// regardless of age. Deferred during the grace window, since a just-restarted
	// node has no usage history and the "oldest" ordering is unreliable.
	if p.MaxN > 0 && len(remaining) > p.MaxN {
		sortByRecency(remaining)
		if now.Before(graceUntil) {
			if dec.NextAgeOut.IsZero() || graceUntil.Before(dec.NextAgeOut) {
				dec.NextAgeOut = graceUntil
			}
		} else {
			for _, r := range remaining[p.MaxN:] {
				dec.Delete = append(dec.Delete, Candidate{
					Ref: r.Ref, Digest: r.Digest, LastUsed: r.effLastUsed(), Reason: "max_n_exceeded",
				})
			}
			remaining = remaining[:p.MaxN]
		}
	}

	// keep-N most-recently-used tags.
	if p.KeepN > 0 && len(remaining) > 0 {
		sortByRecency(remaining)
		n := p.KeepN
		if n > len(remaining) {
			n = len(remaining)
		}
		for _, r := range remaining[:n] {
			dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "keep_n_recent"})
		}
		remaining = remaining[n:]
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
}
