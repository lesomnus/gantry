package retention

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/gantry/internal/down"
)

func evalUntagged(now time.Time, in UntaggedInput, graceUntil time.Time) Decision {
	var dec Decision
	EvaluateUntagged(now, in, graceUntil, &dec)
	return dec
}

func TestEvaluateUntaggedProtectionsAndDeadline(t *testing.T) {
	now := time.Now()
	dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	imgs := []down.UntaggedImage{
		{ID: "sha256:due", RepoDigests: []string{"r/a@" + dg}},
		{ID: "sha256:young", RepoDigests: []string{"r/b@" + dg}},
		{ID: "sha256:run", RepoDigests: []string{"r/c@" + dg}},
		{ID: "sha256:rundg", RepoDigests: []string{"r/d@" + dg}},
		{ID: "sha256:owned", RepoDigests: []string{"r/e@" + dg}},
		{ID: "sha256:pinref", RepoDigests: []string{"r/f@" + dg}},
		{ID: "sha256:pinid", RepoDigests: nil}, // ref-less: pinnable by bare ID only
	}
	ownedRec := Record{Ref: "r/e@" + dg}
	ownedRec.Repo, ownedRec.Tag, ownedRec.Digest = parseRef(ownedRec.Ref)
	in := UntaggedInput{
		Images: imgs,
		FirstSeen: map[string]time.Time{
			"sha256:due":    now.Add(-2 * time.Hour),
			"sha256:young":  now.Add(-10 * time.Minute),
			"sha256:run":    now.Add(-2 * time.Hour),
			"sha256:rundg":  now.Add(-2 * time.Hour),
			"sha256:owned":  now.Add(-2 * time.Hour),
			"sha256:pinref": now.Add(-2 * time.Hour),
			"sha256:pinid":  now.Add(-2 * time.Hour),
		},
		Records: []Record{ownedRec},
		InUse:   map[string]bool{"sha256:run": true, "r/d@" + dg: true},
		Pins: []PinEntry{
			{Value: "r/f@" + dg},    // exact digest-ref pin
			{Value: "sha256:pinid"}, // exact bare-ID pin
		},
		After: time.Hour,
	}
	del, keep := decided(evalUntagged(now, in, time.Time{}))
	if del["sha256:due"] != "untagged" {
		t.Errorf("due image not reaped: del=%v keep=%v", del, keep)
	}
	if keep["sha256:young"] != "untagged_grace" {
		t.Errorf("young image should wait out its grace: %v", keep)
	}
	if keep["sha256:run"] != "in_use" || keep["sha256:rundg"] != "in_use" {
		t.Errorf("in-use (by ID and by digest ref) not protected: %v", keep)
	}
	if keep["sha256:owned"] != "digest_tracked" {
		t.Errorf("digest-tracked image must defer to the rule engine: %v", keep)
	}
	if keep["sha256:pinref"] != "pinned" || keep["sha256:pinid"] != "pinned" {
		t.Errorf("pinned images not protected: %v", keep)
	}
}

func TestEvaluateUntaggedNextAgeOutAndUnseenClock(t *testing.T) {
	now := time.Now()
	in := UntaggedInput{
		Images:    []down.UntaggedImage{{ID: "sha256:new"}},
		FirstSeen: map[string]time.Time{}, // never scanned: clock treated as now
		After:     time.Hour,
	}
	dec := evalUntagged(now, in, time.Time{})
	_, keep := decided(dec)
	if keep["sha256:new"] != "untagged_grace" {
		t.Errorf("unseen image must not be reaped immediately: %v", keep)
	}
	want := now.Add(time.Hour)
	if !dec.NextAgeOut.Equal(want) {
		t.Errorf("next_age_out = %v, want %v", dec.NextAgeOut, want)
	}
}

func TestEvaluateUntaggedDeferredDuringStartupGrace(t *testing.T) {
	now := time.Now()
	graceUntil := now.Add(30 * time.Minute)
	in := UntaggedInput{
		Images:    []down.UntaggedImage{{ID: "sha256:x"}},
		FirstSeen: map[string]time.Time{"sha256:x": now.Add(-2 * time.Hour)},
		After:     time.Hour,
	}
	dec := evalUntagged(now, in, graceUntil)
	_, keep := decided(dec)
	if keep["sha256:x"] != "untagged_grace" {
		t.Errorf("startup grace must defer the reap: %v", keep)
	}
	if !dec.NextAgeOut.Equal(graceUntil) {
		t.Errorf("next_age_out = %v, want grace end %v", dec.NextAgeOut, graceUntil)
	}
}

// The daemon reports familiar names ("nginx@sha256:x") while gantry records
// canonical refs ("index.docker.io/library/nginx@sha256:x"). Ownership and
// exact pins must protect across the two spellings — a digest job for a Docker
// Hub image is exactly this shape.
func TestEvaluateUntaggedMatchesAcrossRefSpellings(t *testing.T) {
	now := time.Now()
	dg := "sha256:0123456789012345678901234567890123456789012345678901234567890123"
	canonical := "index.docker.io/library/nginx@" + dg
	rec := Record{Ref: canonical}
	rec.Repo, rec.Tag, rec.Digest = parseRef(canonical)

	in := UntaggedInput{
		Images:    []down.UntaggedImage{{ID: "sha256:x", RepoDigests: []string{"nginx@" + dg}}},
		FirstSeen: map[string]time.Time{"sha256:x": now.Add(-2 * time.Hour)},
		Records:   []Record{rec},
		After:     time.Hour,
	}
	_, keep := decided(evalUntagged(now, in, time.Time{}))
	if keep["sha256:x"] != "digest_tracked" {
		t.Errorf("canonical record must protect the familiar daemon ref: %v", keep)
	}

	in.Records = nil
	in.Pins = []PinEntry{{Value: canonical}} // exact pin in the canonical spelling
	_, keep = decided(evalUntagged(now, in, time.Time{}))
	if keep["sha256:x"] != "pinned" {
		t.Errorf("a canonical exact pin must protect the familiar daemon ref: %v", keep)
	}
}

func TestEvaluateUntaggedTagPinsCannotProtect(t *testing.T) {
	now := time.Now()
	in := UntaggedInput{
		Images:    []down.UntaggedImage{{ID: "sha256:x", RepoDigests: []string{"r/a@sha256:1"}}},
		FirstSeen: map[string]time.Time{"sha256:x": now.Add(-2 * time.Hour)},
		Pins:      []PinEntry{{Value: "*:stable", Pattern: true}, {Value: "r/a:1"}},
		After:     time.Hour,
	}
	del, _ := decided(evalUntagged(now, in, time.Time{}))
	if del["sha256:x"] != "untagged" {
		t.Errorf("tag-form pins must not protect a tag-less image: del=%v", del)
	}
}

func TestEvaluateUntaggedDisabled(t *testing.T) {
	now := time.Now()
	in := UntaggedInput{
		Images:    []down.UntaggedImage{{ID: "sha256:x"}},
		FirstSeen: map[string]time.Time{"sha256:x": now.Add(-100 * time.Hour)},
		After:     0,
	}
	dec := evalUntagged(now, in, time.Time{})
	if len(dec.Delete)+len(dec.Keep) != 0 {
		t.Errorf("after<=0 must be a no-op, got %+v", dec)
	}
}

// --- index ---

func TestUntaggedFirstSeenSticky(t *testing.T) {
	ix := openTemp(t)
	first := time.Now().Add(-time.Hour).UTC()
	if err := ix.ObserveUntagged("d", "sha256:x", first); err != nil {
		t.Fatal(err)
	}
	if err := ix.ObserveUntagged("d", "sha256:x", time.Now()); err != nil {
		t.Fatal(err)
	}
	es, err := ix.UntaggedEntries("d")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || !es[0].FirstSeen.Equal(first) {
		t.Errorf("first_seen must be write-once: %+v", es)
	}
}

func TestUntaggedDeleteAndCounts(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("d", "r/a:1", time.Now())
	_ = ix.ObserveUntagged("d", "sha256:x", time.Now())
	if nrec, _, nunt, err := ix.Counts("d"); err != nil || nrec != 1 || nunt != 1 {
		t.Errorf("counts = rec %d unt %d (%v), want 1/1", nrec, nunt, err)
	}
	if existed, err := ix.DeleteUntagged("d", "sha256:x"); err != nil || !existed {
		t.Errorf("delete = %v/%v, want existed", existed, err)
	}
	if existed, _ := ix.DeleteUntagged("d", "sha256:x"); existed {
		t.Error("second delete must report not-existed")
	}
}

func TestObserveDoesNotBumpUsage(t *testing.T) {
	ix := openTemp(t)
	used := time.Now().Add(-time.Hour)
	_ = ix.Touch("d", "r/a:1", used)
	_ = ix.Observe("d", "r/a:1", time.Now())
	recs, _ := ix.List("d")
	if len(recs) != 1 || !recs[0].LastUsed.Equal(used) {
		t.Errorf("observe must not touch usage: %+v", recs)
	}
	_ = ix.Observe("d", "r/b:1", used)
	recs, _ = ix.List("d")
	if len(recs) != 2 {
		t.Errorf("observe must create unknown records: %+v", recs)
	}
	for _, r := range recs {
		if r.Ref == "r/b:1" && (!r.LastUsed.IsZero() || !r.FirstSeen.Equal(used)) {
			t.Errorf("observed record must carry first_seen only: %+v", r)
		}
	}
}

// --- manager ---

// reconEng is a fakeEng with the inventory-scan capability.
type reconEng struct {
	fakeEng
	inv     down.Inventory
	reapOK  bool
	reapNop bool // ok without removing anything (image already gone)
	reaped  []string
}

func (f *reconEng) Images(context.Context) (down.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inv, nil
}

func (f *reconEng) ReapUntagged(_ context.Context, id string, _ func(string) bool) (down.RemoveResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.reapOK {
		return down.RemoveResult{}, false, nil
	}
	if f.reapNop {
		return down.RemoveResult{}, true, nil
	}
	f.reaped = append(f.reaped, id)
	return down.RemoveResult{Deleted: []string{id}}, true, nil
}

func TestManagerReapsUntaggedAfterDeadline(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv: down.Inventory{
			Refs:     []string{"r/seed:1"},
			Untagged: []down.UntaggedImage{{ID: "sha256:x", RepoDigests: []string{"r/a@sha256:1"}}},
		},
		reapOK: true,
	}
	m := NewManager([]Store{{
		Name: "d", Engine: eng, Index: ix,
		Rules: blanketRules(Policy{}), UntaggedAfter: 50 * time.Millisecond,
	}})
	u := m.units["d"]

	dec := u.gcOnce(context.Background())
	_, keep := decided(dec)
	if keep["sha256:x"] != "untagged_grace" {
		t.Fatalf("first pass must start the clock, not reap: %+v", dec)
	}
	if recs, _ := ix.List("d"); len(recs) != 1 || recs[0].Ref != "r/seed:1" {
		t.Errorf("scan must seed unknown tagged refs: %+v", recs)
	}

	time.Sleep(60 * time.Millisecond)
	dec = u.gcOnce(context.Background())
	del, _ := decided(dec)
	if del["sha256:x"] != "untagged" {
		t.Fatalf("second pass past the deadline must reap: %+v", dec)
	}
	eng.mu.Lock()
	reaped := append([]string(nil), eng.reaped...)
	eng.mu.Unlock()
	if len(reaped) != 1 || reaped[0] != "sha256:x" {
		t.Errorf("engine reaped = %v", reaped)
	}
	if es, _ := ix.UntaggedEntries("d"); len(es) != 0 {
		t.Errorf("reaped entry must leave the index: %+v", es)
	}
}

func TestManagerReapSkipHoldsEntry(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv:     down.Inventory{Untagged: []down.UntaggedImage{{ID: "sha256:x"}}},
		reapOK:  false, // e.g. re-tagged or a container appeared between plan and apply
	}
	m := NewManager([]Store{{
		Name: "d", Engine: eng, Index: ix,
		Rules: blanketRules(Policy{}), UntaggedAfter: time.Millisecond,
	}})
	u := m.units["d"]
	u.reconcile(context.Background())
	time.Sleep(5 * time.Millisecond)
	dec, err := u.plan(context.Background(), u.rules, u.untaggedAfter, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := u.apply(context.Background(), dec)
	if len(res.Skipped) != 1 || res.Skipped[0] != "sha256:x" {
		t.Fatalf("apply = %+v, want the image skipped", res)
	}
	if es, _ := ix.UntaggedEntries("d"); len(es) != 1 {
		t.Errorf("skipped entry must stay tracked: %+v", es)
	}
}

func TestManagerReconcileDropsRetaggedEntry(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv:     down.Inventory{Untagged: []down.UntaggedImage{{ID: "sha256:x"}}},
	}
	m := NewManager([]Store{{Name: "d", Engine: eng, Index: ix, UntaggedAfter: time.Hour}})
	u := m.units["d"]
	u.reconcile(context.Background())
	if es, _ := ix.UntaggedEntries("d"); len(es) != 1 {
		t.Fatalf("entry not tracked: %+v", es)
	}
	eng.mu.Lock()
	eng.inv = down.Inventory{} // the image regained a tag (or was removed)
	eng.mu.Unlock()
	u.reconcile(context.Background())
	if es, _ := ix.UntaggedEntries("d"); len(es) != 0 {
		t.Errorf("re-tagged entry must be dropped: %+v", es)
	}
}

func TestManagerPlanIsReadOnly(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv:     down.Inventory{Untagged: []down.UntaggedImage{{ID: "sha256:x"}}},
	}
	m := NewManager([]Store{{Name: "d", Engine: eng, Index: ix, UntaggedAfter: time.Hour}})
	dec, err := m.Plan(context.Background(), "d", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, keep := decided(dec)
	if keep["sha256:x"] != "untagged_grace" {
		t.Errorf("dry-run must show the pending reap: %+v", dec)
	}
	if es, _ := ix.UntaggedEntries("d"); len(es) != 0 {
		t.Errorf("a dry-run must not start reap clocks: %+v", es)
	}
}

func TestManagerPlanOverrideControlsReaper(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv:     down.Inventory{Untagged: []down.UntaggedImage{{ID: "sha256:x"}}},
	}
	m := NewManager([]Store{{Name: "d", Engine: eng, Index: ix, UntaggedAfter: time.Hour}})
	_ = m.units["d"].ix.ObserveUntagged("d", "sha256:x", time.Now().Add(-2*time.Hour))

	// An override body that does not set untagged_after turns the reaper off
	// for the call, like every other unset override field.
	dec, err := m.Plan(context.Background(), "d", &Policy{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if del, keep := decided(dec); del["sha256:x"] != "" || keep["sha256:x"] != "" {
		t.Errorf("override without untagged_after must skip the reaper: del=%v keep=%v", del, keep)
	}

	dec, err = m.Plan(context.Background(), "d", &Policy{UntaggedAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if del, _ := decided(dec); del["sha256:x"] != "untagged" {
		t.Errorf("override with untagged_after must reap: %+v", dec)
	}
}

func TestManagerPlanOverrideCannotEnableDisabledReaper(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv:     down.Inventory{Untagged: []down.UntaggedImage{{ID: "sha256:x"}}},
	}
	m := NewManager([]Store{{Name: "d", Engine: eng, Index: ix, UntaggedAfter: 0}}) // "0s": must never reap
	if _, err := m.Plan(context.Background(), "d", &Policy{UntaggedAfter: time.Hour}); err == nil {
		t.Error("an override must not enable reaping on a store configured off")
	}
}

func TestManagerReapAlreadyGoneNotCounted(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv:     down.Inventory{Untagged: []down.UntaggedImage{{ID: "sha256:x"}}},
		reapOK:  true,
		reapNop: true, // engine reports ok with nothing removed (already gone)
	}
	m := NewManager([]Store{{Name: "d", Engine: eng, Index: ix, UntaggedAfter: time.Millisecond}})
	u := m.units["d"]
	u.reconcile(context.Background())
	time.Sleep(5 * time.Millisecond)
	dec, err := u.plan(context.Background(), u.rules, u.untaggedAfter, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := u.apply(context.Background(), dec)
	if len(res.Reaped) != 0 {
		t.Errorf("an already-gone image must not count as reaped: %+v", res)
	}
	if es, _ := ix.UntaggedEntries("d"); len(es) != 0 {
		t.Errorf("the index must still converge: %+v", es)
	}
}

func TestManagerReapSkipsPurgedEntry(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{
		fakeEng: fakeEng{name: "d"},
		inv:     down.Inventory{Untagged: []down.UntaggedImage{{ID: "sha256:x"}}},
		reapOK:  true,
	}
	m := NewManager([]Store{{Name: "d", Engine: eng, Index: ix, UntaggedAfter: time.Millisecond}})
	u := m.units["d"]
	u.reconcile(context.Background())
	time.Sleep(5 * time.Millisecond)
	dec, err := u.plan(context.Background(), u.rules, u.untaggedAfter, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A DELETE /image purge lands between plan and apply: the stale decision
	// must not reap.
	if _, err := m.DeleteRecord("d", "sha256:x"); err != nil {
		t.Fatal(err)
	}
	res := u.apply(context.Background(), dec)
	if len(res.Skipped) != 1 || len(res.Reaped) != 0 {
		t.Errorf("a purged reap clock must cancel the reap: %+v", res)
	}
	eng.mu.Lock()
	reaped := len(eng.reaped)
	eng.mu.Unlock()
	if reaped != 0 {
		t.Error("the engine must not be asked to reap a purged entry")
	}
}

func TestManagerDeleteRecordPurgesUntagged(t *testing.T) {
	ix := openTemp(t)
	eng := &reconEng{fakeEng: fakeEng{name: "d"}}
	m := NewManager([]Store{{Name: "d", Engine: eng, Index: ix, UntaggedAfter: time.Hour}})
	_ = ix.ObserveUntagged("d", "sha256:x", time.Now())
	existed, err := m.DeleteRecord("d", "sha256:x")
	if err != nil || !existed {
		t.Fatalf("delete = %v/%v, want the untagged entry purged", existed, err)
	}
	if es, _ := ix.UntaggedEntries("d"); len(es) != 0 {
		t.Errorf("entry still present: %+v", es)
	}
}
