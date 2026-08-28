package retention

import (
	"path"
	"sort"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/lesomnus/gantry/internal/down"
)

// matchPin reports whether a pin (an exact reference or a doublestar pattern)
// protects the record. A pattern is tried against the full ref, the name:tag
// short form (last repo segment), and the bare tag — so "cache.local/a/app:1",
// "*:stable", and "prod-*" all match as intended.
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

// digestGroups buckets records by content so keep-N/max-N count by digest, not
// by tag: records sharing a resolved Digest form one group (multiple tags of the
// same image count once), and a record with no resolved digest is its own group.
// Groups are returned most-recently-used first (by the group's most-recent
// record, tie-broken by that record's Ref); each group is itself recency-sorted.
func digestGroups(recs []Record) [][]Record {
	byKey := map[string]int{} // key -> index in groups
	var groups [][]Record
	for _, r := range recs {
		k := r.Digest
		if k == "" {
			k = "\x00ref:" + r.Ref // no digest → counted individually
		}
		if i, ok := byKey[k]; ok {
			groups[i] = append(groups[i], r)
		} else {
			byKey[k] = len(groups)
			groups = append(groups, []Record{r})
		}
	}
	for _, g := range groups {
		sortByRecency(g)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		ri, rj := groups[i][0], groups[j][0]
		ti, tj := ri.effLastUsed(), rj.effLastUsed()
		if ti.Equal(tj) {
			return ri.Ref < rj.Ref
		}
		return ti.After(tj)
	})
	return groups
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

	var age, idle *time.Duration
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
		if idle == nil {
			idle = r.MaxIdle
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
	if idle != nil {
		p.MaxIdle = *idle
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
//  3. idle     — delete those idle longer than MaxIdle regardless of keep-N/max-N
//     (a hard cap; deferred during the grace window)
//  4. max-N    — keep only the MaxN most-recently-used tags; delete the oldest
//     beyond the cap even if not yet stale (deferred during the grace window)
//  5. keep-N   — the KeepN most-recently-used tags
//  6. age      — delete those whose last-used age exceeds MaxAge (deferred during
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

// UntaggedInput is one store's inventory-scan state fed to EvaluateUntagged.
type UntaggedInput struct {
	Images    []down.UntaggedImage // live scan: images with no tags
	FirstSeen map[string]time.Time // image ID -> first observed untagged (missing => treated as now)
	Records   []Record             // live index records, for digest-ref ownership
	InUse     map[string]bool      // running containers' refs and image IDs
	Pins      []PinEntry           // API pins plus rule pins (rule pins wrapped as patterns)
	After     time.Duration        // reap delay from first observation; <=0 disables
}

// EvaluateUntagged appends the untagged-image reap decision to dec. Untagged
// images bypass the per-repo rules — they have no tag for a rule to manage —
// and are deleted After their first observation with no tags, deferred by the
// same startup grace window as age GC. Protections, in order:
//  1. digest_tracked — a live index record exists for one of the image's
//     repo@digest refs: the image is deliberately digest-pinned (a digest job
//     or manual digest pull) and the rule engine owns it.
//  2. in_use — a running container references the image ID or a digest ref.
//  3. pinned — a pin protects one of the digest refs (or the bare image ID).
//     Tag-form pins cannot protect an image that lost its tags; pin repo@digest
//     to protect content.
func EvaluateUntagged(now time.Time, in UntaggedInput, graceUntil time.Time, dec *Decision) {
	if in.After <= 0 {
		return
	}
	// The daemon reports familiar names ("nginx@sha256:x") while gantry's refs
	// are canonical ("index.docker.io/library/nginx@sha256:x"); ownership is
	// matched on the parseRef-canonical (repo, digest) pair so both spellings
	// of a Docker Hub reference collide.
	owned := make(map[string]bool, len(in.Records))
	for _, r := range in.Records {
		if r.Digest != "" && r.Tag == "" {
			owned[r.Repo+"@"+r.Digest] = true
		}
	}
	ownedBy := func(img down.UntaggedImage) bool {
		for _, d := range img.RepoDigests {
			if repo, _, dg := parseRef(d); dg != "" && owned[repo+"@"+dg] {
				return true
			}
		}
		return false
	}
	for _, img := range in.Images {
		keep := func(reason string) {
			dec.Keep = append(dec.Keep, Kept{Ref: img.ID, Reason: reason})
		}
		if ownedBy(img) {
			keep("digest_tracked")
			continue
		}
		if in.InUse[img.ID] || anyKey(in.InUse, img.RepoDigests) {
			keep("in_use")
			continue
		}
		if pinnedUntagged(img, in.Pins) {
			keep("pinned")
			continue
		}
		fs := in.FirstSeen[img.ID]
		if fs.IsZero() {
			fs = now // not yet persisted (e.g. a dry-run before the first scheduled scan)
		}
		deletableAt := fs.Add(in.After)
		if graceUntil.After(deletableAt) {
			deletableAt = graceUntil
		}
		if !now.Before(deletableAt) {
			// Record a repo@digest that still named the image (and its digest) so
			// the audit event says what was reaped, not just an opaque image ID.
			ref, digest := img.ID, ""
			if len(img.RepoDigests) > 0 {
				ref = img.RepoDigests[0]
				if _, _, dg := parseRef(ref); dg != "" {
					digest = dg
				}
			}
			dec.Delete = append(dec.Delete, Candidate{
				Ref: ref, ImageID: img.ID, Digest: digest, LastUsed: fs, Reason: "untagged",
			})
			continue
		}
		keep("untagged_grace")
		if dec.NextAgeOut.IsZero() || deletableAt.Before(dec.NextAgeOut) {
			dec.NextAgeOut = deletableAt
		}
	}
}

func anyKey(m map[string]bool, keys []string) bool {
	for _, k := range keys {
		if m[k] {
			return true
		}
	}
	return false
}

// pinnedUntagged reports whether any pin protects the untagged image, matched
// against a synthesized record per repo@digest ref and the bare image ID. Each
// digest ref is tried in the daemon's spelling and the canonical one, so an
// exact pin written either way ("nginx@sha256:x" or
// "index.docker.io/library/nginx@sha256:x") protects.
func pinnedUntagged(img down.UntaggedImage, pins []PinEntry) bool {
	recs := make([]Record, 0, 2*len(img.RepoDigests)+1)
	for _, d := range img.RepoDigests {
		r := Record{Ref: d}
		r.Repo, r.Tag, r.Digest = parseRef(d)
		recs = append(recs, r)
		if canon := r.Repo + "@" + r.Digest; r.Digest != "" && canon != d {
			recs = append(recs, Record{Ref: canon, Repo: r.Repo, Digest: r.Digest})
		}
	}
	recs = append(recs, Record{Ref: img.ID})
	for _, pin := range pins {
		for _, r := range recs {
			if pin.protects(r) {
				return true
			}
		}
	}
	return false
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

	// Hard idle cap: an image unused longer than MaxIdle is deleted regardless of
	// keep-N / max-N — only in-use and pins (checked above) protect it — so a
	// settled-but-ancient tag does not linger forever. Deferred during the grace
	// window like age GC.
	if p.MaxIdle > 0 {
		kept := remaining[:0]
		for _, r := range remaining {
			idleOut := r.effLastUsed().Add(p.MaxIdle)
			deletableAt := idleOut
			if graceUntil.After(deletableAt) {
				deletableAt = graceUntil
			}
			if !now.Before(deletableAt) {
				dec.Delete = append(dec.Delete, Candidate{
					Ref: r.Ref, Digest: r.Digest, LastUsed: r.effLastUsed(), Reason: "idle_exceeded",
				})
				continue
			}
			kept = append(kept, r)
			if !now.Before(idleOut) { // would idle out, but grace holds it
				if dec.NextAgeOut.IsZero() || deletableAt.Before(dec.NextAgeOut) {
					dec.NextAgeOut = deletableAt
				}
			}
		}
		remaining = kept
	}

	// keep-N / max-N count by CONTENT (digest): tags sharing a digest count once,
	// so keeping "the 2 most recent" keeps the 2 newest images, not 2 tags that may
	// point at the same blob. A record with no resolved digest counts individually.
	groups := digestGroups(remaining)

	// max-N cap: keep at most MaxN digest-groups, deleting every tag in the oldest
	// groups beyond the cap regardless of age. Deferred during the grace window,
	// since a just-restarted node has no usage history and ordering is unreliable.
	if p.MaxN > 0 && len(groups) > p.MaxN {
		if now.Before(graceUntil) {
			if dec.NextAgeOut.IsZero() || graceUntil.Before(dec.NextAgeOut) {
				dec.NextAgeOut = graceUntil
			}
		} else {
			for _, g := range groups[p.MaxN:] {
				for _, r := range g {
					dec.Delete = append(dec.Delete, Candidate{
						Ref: r.Ref, Digest: r.Digest, LastUsed: r.effLastUsed(), Reason: "max_n_exceeded",
					})
				}
			}
			groups = groups[:p.MaxN]
		}
	}

	// keep-N most-recently-used digest-groups.
	if p.KeepN > 0 && len(groups) > 0 {
		n := p.KeepN
		if n > len(groups) {
			n = len(groups)
		}
		for _, g := range groups[:n] {
			for _, r := range g {
				dec.Keep = append(dec.Keep, Kept{Ref: r.Ref, Reason: "keep_n_recent"})
			}
		}
		groups = groups[n:]
	}

	// whatever survived keep-N proceeds to age evaluation.
	remaining = remaining[:0]
	for _, g := range groups {
		remaining = append(remaining, g...)
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
