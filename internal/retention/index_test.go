package retention

import (
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func openTemp(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func TestIndexTouchMergeAndParse(t *testing.T) {
	ix := openTemp(t)
	t0 := time.Now().Add(-time.Hour)
	t1 := time.Now()
	if err := ix.Touch("docker", "ghcr.io/lesomnus/a:foo", t1); err != nil {
		t.Fatal(err)
	}
	if err := ix.Touch("docker", "ghcr.io/lesomnus/a:foo", t0); err != nil { // older -> ignored
		t.Fatal(err)
	}
	recs, _ := ix.List("docker")
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	if !r.LastUsed.Equal(t1) {
		t.Errorf("LastUsed = %v, want max(%v,%v)", r.LastUsed, t0, t1)
	}
	if r.Repo != "ghcr.io/lesomnus/a" || r.Tag != "foo" {
		t.Errorf("parse = repo:%q tag:%q", r.Repo, r.Tag)
	}
}

func TestIndexDistributedFallback(t *testing.T) {
	ix := openTemp(t)
	ts := time.Now()
	_ = ix.Distributed("docker", "cache/app:1", ts)
	recs, _ := ix.List("docker")
	r := recs[0]
	if !r.LastUsed.IsZero() {
		t.Errorf("LastUsed should stay zero, got %v", r.LastUsed)
	}
	if !r.LastDistributed.Equal(ts) {
		t.Errorf("LastDistributed = %v", r.LastDistributed)
	}
	if got := r.effLastUsed(); !got.Equal(ts) {
		t.Errorf("effLastUsed = %v, want distributed time", got)
	}
}

func TestIndexPinAndDelete(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("docker", "cache/app:1", time.Now())
	_ = ix.Pin("docker", "cache/app:1", false)
	recs, _ := ix.List("docker")
	if len(recs) != 1 || !recs[0].Pinned {
		t.Fatalf("pin not reflected: %+v", recs)
	}
	if pins, _ := ix.Pins("docker"); len(pins) != 1 || pins[0].Value != "cache/app:1" {
		t.Errorf("pins = %v", pins)
	}
	_ = ix.Unpin("docker", "cache/app:1")
	recs, _ = ix.List("docker")
	if recs[0].Pinned {
		t.Error("still pinned after unpin")
	}
	_ = ix.Delete("docker", "cache/app:1")
	if recs, _ := ix.List("docker"); len(recs) != 0 {
		t.Errorf("record not deleted: %+v", recs)
	}
}

func TestIndexEnginesIsolated(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("a", "x:1", time.Now())
	_ = ix.Touch("b", "y:1", time.Now())
	if ra, _ := ix.List("a"); len(ra) != 1 || ra[0].Ref != "x:1" {
		t.Errorf("engine a = %+v", ra)
	}
	if rb, _ := ix.List("b"); len(rb) != 1 || rb[0].Ref != "y:1" {
		t.Errorf("engine b = %+v", rb)
	}
}

func TestPinsEntriesAndLegacyMarkers(t *testing.T) {
	ix := openTemp(t)
	if err := ix.Pin("d", "*:stable", true); err != nil {
		t.Fatal(err)
	}
	// A pin written before entries carried metadata: a raw marker byte.
	err := ix.db.Update(func(tx *bolt.Tx) error {
		b, err := sub(tx, bktPin, "d")
		if err != nil {
			return err
		}
		return b.Put([]byte("cache.local/a/app:1"), []byte{1})
	})
	if err != nil {
		t.Fatal(err)
	}
	pins, err := ix.Pins("d")
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 2 {
		t.Fatalf("pins = %+v, want 2 entries", pins)
	}
	by_value := map[string]PinEntry{}
	for _, p := range pins {
		by_value[p.Value] = p
	}
	if e, ok := by_value["*:stable"]; !ok || e.At.IsZero() {
		t.Errorf("new pin should carry a timestamp: %+v", e)
	}
	if e, ok := by_value["cache.local/a/app:1"]; !ok || !e.At.IsZero() {
		t.Errorf("legacy pin should be listed with a zero timestamp: %+v", e)
	}
}

func TestListMarksPatternPinned(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("d", "cache.local/a/app:stable", time.Now())
	_ = ix.Touch("d", "cache.local/a/app:dev", time.Now())
	if err := ix.Pin("d", "*:stable", true); err != nil {
		t.Fatal(err)
	}
	recs, err := ix.List("d")
	if err != nil {
		t.Fatal(err)
	}
	pinned_refs := map[string]bool{}
	for _, r := range recs {
		pinned_refs[r.Ref] = r.Pinned
	}
	if !pinned_refs["cache.local/a/app:stable"] || pinned_refs["cache.local/a/app:dev"] {
		t.Errorf("pattern pin should mark only the stable tag: %v", pinned_refs)
	}
}

func TestExactPinDoesNotOverreach(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("d", "cache.local/team/app:1", time.Now())
	_ = ix.Touch("d", "other.io/x/app:1", time.Now())
	// An exact-ref pin must only protect its own ref — never short-name or
	// tag-match records in other repos.
	if err := ix.Pin("d", "cache.local/team/app:1", false); err != nil {
		t.Fatal(err)
	}
	recs, err := ix.List("d")
	if err != nil {
		t.Fatal(err)
	}
	pinned_refs := map[string]bool{}
	for _, r := range recs {
		pinned_refs[r.Ref] = r.Pinned
	}
	if !pinned_refs["cache.local/team/app:1"] || pinned_refs["other.io/x/app:1"] {
		t.Errorf("exact pin overreach: %v", pinned_refs)
	}
	// The same value as a PATTERN pin short-name matches both.
	if err := ix.Pin("d", "app:1", true); err != nil {
		t.Fatal(err)
	}
	recs, _ = ix.List("d")
	for _, r := range recs {
		if !r.Pinned {
			t.Errorf("pattern pin app:1 should protect %s via its short name", r.Ref)
		}
	}
}
