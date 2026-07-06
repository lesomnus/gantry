package retention

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func rec(ref, repo string, lastUsed time.Time) Record {
	return Record{Ref: ref, Repo: repo, LastUsed: lastUsed, FirstSeen: lastUsed}
}

func decided(d Decision) (del map[string]string, keep map[string]string) {
	del, keep = map[string]string{}, map[string]string{}
	for _, c := range d.Delete {
		del[c.Ref] = c.Reason
	}
	for _, k := range d.Keep {
		keep[k.Ref] = k.Reason
	}
	return
}

func TestEvaluateInUseAndPinBeatAge(t *testing.T) {
	now := time.Now()
	old := now.Add(-100 * time.Hour)
	recs := []Record{
		rec("r/a:1", "r/a", old),
		{Ref: "r/a:2", Repo: "r/a", LastUsed: old, Pinned: true},
		rec("r/a:3", "r/a", old),
	}
	inUse := map[string]bool{"r/a:1": true}
	d := Evaluate(now, recs, inUse, Policy{MaxAge: time.Hour, KeepN: 0}, time.Time{})
	del, keep := decided(d)
	if keep["r/a:1"] != "in_use" {
		t.Errorf("in-use not protected: %v", keep)
	}
	if keep["r/a:2"] != "pinned" {
		t.Errorf("pinned not protected: %v", keep)
	}
	if del["r/a:3"] != "age_exceeded" {
		t.Errorf("old unprotected should be deleted: del=%v keep=%v", del, keep)
	}
}

func TestEvaluateKeepNPerRepo(t *testing.T) {
	now := time.Now()
	// repo r/a: 4 tags at decreasing recency, all old; KeepN=2 keeps the 2 most recent.
	mk := func(tag string, agoHours int) Record {
		return rec("r/a:"+tag, "r/a", now.Add(-time.Duration(agoHours)*time.Hour))
	}
	recs := []Record{mk("foo", 200), mk("bar", 10), mk("baz", 5), mk("qux", 1)}
	d := Evaluate(now, recs, nil, Policy{MaxAge: time.Hour, KeepN: 2}, time.Time{})
	del, keep := decided(d)
	if keep["r/a:qux"] != "keep_n_recent" || keep["r/a:baz"] != "keep_n_recent" {
		t.Errorf("two most-recent tags not kept: keep=%v", keep)
	}
	if del["r/a:bar"] != "age_exceeded" || del["r/a:foo"] != "age_exceeded" {
		t.Errorf("older tags beyond N not deleted: del=%v", del)
	}
}

func TestEvaluateKeepNIsPerRepoNotGlobal(t *testing.T) {
	now := time.Now()
	recs := []Record{
		rec("r/a:1", "r/a", now.Add(-50*time.Hour)),
		rec("r/b:1", "r/b", now.Add(-50*time.Hour)),
	}
	d := Evaluate(now, recs, nil, Policy{MaxAge: time.Hour, KeepN: 1}, time.Time{})
	_, keep := decided(d)
	if keep["r/a:1"] != "keep_n_recent" || keep["r/b:1"] != "keep_n_recent" {
		t.Errorf("keep-N should apply per repo: keep=%v", keep)
	}
}

func TestEvaluateAgeBoundaryAndYoungKept(t *testing.T) {
	now := time.Now()
	recs := []Record{
		rec("r/a:young", "r/a", now.Add(-30*time.Minute)),
		rec("r/a:old", "r/a", now.Add(-2*time.Hour)),
	}
	d := Evaluate(now, recs, nil, Policy{MaxAge: time.Hour, KeepN: 0}, time.Time{})
	del, keep := decided(d)
	if del["r/a:old"] != "age_exceeded" {
		t.Errorf("old should be deleted: %v", del)
	}
	if keep["r/a:young"] != "within_max_age" {
		t.Errorf("young should be kept: %v", keep)
	}
	if d.NextAgeOut.IsZero() {
		t.Error("expected a next age-out deadline for the young record")
	}
}

func TestEvaluateGraceHoldsDeletion(t *testing.T) {
	now := time.Now()
	graceUntil := now.Add(time.Hour) // still in grace
	recs := []Record{rec("r/a:old", "r/a", now.Add(-100*time.Hour))}
	d := Evaluate(now, recs, nil, Policy{MaxAge: time.Hour, KeepN: 0}, graceUntil)
	del, keep := decided(d)
	if len(del) != 0 {
		t.Errorf("grace should hold deletion: del=%v", del)
	}
	if keep["r/a:old"] != "grace" {
		t.Errorf("expected grace reason: keep=%v", keep)
	}
	if !d.NextAgeOut.Equal(graceUntil) {
		t.Errorf("next age-out should be graceUntil, got %v", d.NextAgeOut)
	}
}

func TestEvaluateMaxAgeZeroDisablesAgeGC(t *testing.T) {
	now := time.Now()
	recs := []Record{rec("r/a:old", "r/a", now.Add(-1000*time.Hour))}
	d := Evaluate(now, recs, nil, Policy{MaxAge: 0, KeepN: 0}, time.Time{})
	del, keep := decided(d)
	if len(del) != 0 || keep["r/a:old"] != "age_gc_disabled" {
		t.Errorf("max_age=0 should keep all: del=%v keep=%v", del, keep)
	}
}

func TestDecisionSerializesNextAgeOut(t *testing.T) {
	at := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	b, err := json.Marshal(Decision{NextAgeOut: at})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"next_age_out":"2026-07-02T12:00:00Z"`) {
		t.Errorf("next_age_out missing: %s", b)
	}
	b, err = json.Marshal(Decision{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "next_age_out") {
		t.Errorf("zero next_age_out should be omitted: %s", b)
	}
}

func TestEvaluateMaxNCapDeletesOldestBeyondCap(t *testing.T) {
	now := time.Now()
	mk := func(tag string, agoMin int) Record {
		return rec("r/a:"+tag, "r/a", now.Add(-time.Duration(agoMin)*time.Minute))
	}
	// Five recent (non-stale) tags; MaxN=3 keeps the 3 newest, deletes the 2
	// oldest even though none exceed max_age.
	recs := []Record{mk("t1", 1), mk("t2", 2), mk("t3", 3), mk("t4", 4), mk("t5", 5)}
	d := Evaluate(now, recs, nil, Policy{MaxAge: 1000 * time.Hour, MaxN: 3}, time.Time{})
	del, keep := decided(d)
	for _, r := range []string{"r/a:t1", "r/a:t2", "r/a:t3"} {
		if keep[r] == "" || del[r] != "" {
			t.Errorf("%s should be kept within cap: del=%v keep=%v", r, del, keep)
		}
	}
	if del["r/a:t4"] != "max_n_exceeded" || del["r/a:t5"] != "max_n_exceeded" {
		t.Errorf("oldest beyond cap should be max_n_exceeded: del=%v", del)
	}
}

func TestEvaluateMaxNIsPerRepo(t *testing.T) {
	now := time.Now()
	mk := func(ref, repo string, agoMin int) Record {
		return rec(ref, repo, now.Add(-time.Duration(agoMin)*time.Minute))
	}
	recs := []Record{
		mk("r/a:1", "r/a", 1), mk("r/a:2", "r/a", 2), mk("r/a:3", "r/a", 3),
		mk("r/b:1", "r/b", 1), mk("r/b:2", "r/b", 2), mk("r/b:3", "r/b", 3),
	}
	d := Evaluate(now, recs, nil, Policy{MaxAge: 1000 * time.Hour, MaxN: 2}, time.Time{})
	del, _ := decided(d)
	if del["r/a:3"] != "max_n_exceeded" || del["r/b:3"] != "max_n_exceeded" {
		t.Errorf("each repo should cap independently: del=%v", del)
	}
	if len(del) != 2 {
		t.Errorf("only the oldest per repo should be deleted: del=%v", del)
	}
}

func TestEvaluateMaxNExcludesProtected(t *testing.T) {
	now := time.Now()
	recs := []Record{
		rec("r/a:live", "r/a", now.Add(-1*time.Minute)),
		{Ref: "r/a:pin", Repo: "r/a", LastUsed: now.Add(-2 * time.Minute), FirstSeen: now, Pinned: true},
		rec("r/a:new", "r/a", now.Add(-3*time.Minute)),
		rec("r/a:old", "r/a", now.Add(-4*time.Minute)),
	}
	inUse := map[string]bool{"r/a:live": true}
	// MaxN=1 counts only the 2 non-protected tags (new, old): keep newest, delete
	// oldest. in-use and pinned are always kept and do not consume the budget.
	d := Evaluate(now, recs, inUse, Policy{MaxAge: 1000 * time.Hour, MaxN: 1}, time.Time{})
	del, keep := decided(d)
	if keep["r/a:live"] != "in_use" || keep["r/a:pin"] != "pinned" {
		t.Errorf("protected tags must remain: keep=%v", keep)
	}
	if del["r/a:new"] != "" {
		t.Errorf("newest non-protected within cap should be kept: del=%v", del)
	}
	if del["r/a:old"] != "max_n_exceeded" {
		t.Errorf("oldest non-protected beyond cap should be deleted: del=%v", del)
	}
}

func TestEvaluateMaxNDeferredDuringGrace(t *testing.T) {
	now := time.Now()
	graceUntil := now.Add(time.Hour)
	mk := func(tag string, agoMin int) Record {
		return rec("r/a:"+tag, "r/a", now.Add(-time.Duration(agoMin)*time.Minute))
	}
	recs := []Record{mk("t1", 1), mk("t2", 2), mk("t3", 3)}
	d := Evaluate(now, recs, nil, Policy{MaxAge: 1000 * time.Hour, MaxN: 1}, graceUntil)
	del, _ := decided(d)
	if len(del) != 0 {
		t.Errorf("cap should be deferred during grace: del=%v", del)
	}
	if !d.NextAgeOut.Equal(graceUntil) {
		t.Errorf("next age-out should be graceUntil, got %v", d.NextAgeOut)
	}
}

func TestEvaluateMaxNComposesWithKeepNAndAge(t *testing.T) {
	now := time.Now()
	mk := func(tag string, ago time.Duration) Record {
		return rec("r/a:"+tag, "r/a", now.Add(-ago))
	}
	// Ranked by recency r1..r5. MaxN=3 caps to top 3 (deletes r4,r5); of those,
	// KeepN=2 protects r1,r2; r3 is stale so age deletes it.
	recs := []Record{
		mk("r1", 10*time.Minute),
		mk("r2", 20*time.Minute),
		mk("r3", 2*time.Hour),
		mk("r4", 3*time.Hour),
		mk("r5", 4*time.Hour),
	}
	d := Evaluate(now, recs, nil, Policy{MaxAge: time.Hour, KeepN: 2, MaxN: 3}, time.Time{})
	del, keep := decided(d)
	if keep["r/a:r1"] != "keep_n_recent" || keep["r/a:r2"] != "keep_n_recent" {
		t.Errorf("top keep_n should be protected: keep=%v", keep)
	}
	if del["r/a:r3"] != "age_exceeded" {
		t.Errorf("rank-3 within cap but stale should be age_exceeded: del=%v", del)
	}
	if del["r/a:r4"] != "max_n_exceeded" || del["r/a:r5"] != "max_n_exceeded" {
		t.Errorf("beyond cap should be max_n_exceeded: del=%v", del)
	}
}

func TestEvaluateMaxNNoOpAtOrBelowCap(t *testing.T) {
	now := time.Now()
	mk := func(tag string, agoMin int) Record {
		return rec("r/a:"+tag, "r/a", now.Add(-time.Duration(agoMin)*time.Minute))
	}
	recs := []Record{mk("t1", 1), mk("t2", 2)}
	d := Evaluate(now, recs, nil, Policy{MaxAge: 1000 * time.Hour, MaxN: 2}, time.Time{})
	if del, _ := decided(d); len(del) != 0 {
		t.Errorf("at cap should delete nothing: del=%v", del)
	}
}

func TestMatchPin(t *testing.T) {
	r := Record{Ref: "cache.local/team/app:stable", Repo: "cache.local/team/app", Tag: "stable"}
	cases := map[string]bool{
		"cache.local/team/app:stable": true,  // exact ref
		"*:stable":                    true,  // name:tag short form
		"app:*":                       true,  // short-form name glob
		"stable":                      true,  // bare tag
		"prod-*":                      false, // tag glob, no match
		"cache.local/team/app:1.2":    false,
		"**":                          true, // matches the full ref
	}
	for pin, want := range cases {
		if got := matchPin(pin, r); got != want {
			t.Errorf("matchPin(%q) = %v, want %v", pin, got, want)
		}
	}
	prod := Record{Ref: "cache.local/team/app:prod-3", Repo: "cache.local/team/app", Tag: "prod-3"}
	if !matchPin("prod-*", prod) {
		t.Error("prod-* should pin tag prod-3")
	}
}

func TestEvaluatePatternPins(t *testing.T) {
	now := time.Now()
	recs := []Record{
		{Ref: "cache.local/a/app:stable", Repo: "cache.local/a/app", Tag: "stable", LastUsed: now.Add(-100 * time.Hour), FirstSeen: now.Add(-100 * time.Hour)},
		{Ref: "cache.local/a/app:old", Repo: "cache.local/a/app", Tag: "old", LastUsed: now.Add(-100 * time.Hour), FirstSeen: now.Add(-100 * time.Hour)},
	}
	d := Evaluate(now, recs, nil, Policy{MaxAge: time.Hour, Pins: []string{"*:stable"}}, time.Time{})
	del, keep := decided(d)
	if keep["cache.local/a/app:stable"] != "pinned" {
		t.Errorf("pattern pin should protect the stable tag: keep=%v", keep)
	}
	if del["cache.local/a/app:old"] != "age_exceeded" {
		t.Errorf("unpinned old tag should be deleted: del=%v", del)
	}
}
