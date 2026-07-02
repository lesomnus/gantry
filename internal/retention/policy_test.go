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
