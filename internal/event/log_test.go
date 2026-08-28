package event

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T, cap int) *Log {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "evt.db"), cap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestAppendListNewestFirst(t *testing.T) {
	l := openTemp(t, 100)
	for _, ref := range []string{"a:1", "b:1", "c:1"} {
		if err := l.Append(Event{Type: JobAdmitted, Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Ref != "c:1" || got[2].Ref != "a:1" {
		t.Fatalf("list = %+v, want newest-first", got)
	}
	if got[0].Seq <= got[1].Seq {
		t.Error("seq must be monotonic")
	}
	if got[0].DateCreated.IsZero() {
		t.Error("DateCreated must be stamped")
	}
}

func TestListFilters(t *testing.T) {
	l := openTemp(t, 100)
	_ = l.Append(Event{Type: JobDone, Store: "eng", Ref: "a:1", State: "done"})
	_ = l.Append(Event{Type: GCApplied, Store: "eng"})
	_ = l.Append(Event{Type: JobDone, Store: "other", Ref: "b:1", State: "failed"})

	cases := map[Filter]int{
		{Type: JobDone}:     2,
		{Store: "eng"}:      2,
		{Ref: "a:1"}:        1,
		{State: "failed"}:   1,
		{Type: GCApplied}:   1,
		{Type: ImagePulled}: 0,
	}
	for f, want := range cases {
		got, err := l.List(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Errorf("filter %+v = %d, want %d", f, len(got), want)
		}
	}
}

func TestRingEviction(t *testing.T) {
	l := openTemp(t, 3)
	for i := 0; i < 10; i++ {
		if err := l.Append(Event{Type: JobAdmitted, Ref: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("kept %d entries, want cap 3", len(got))
	}
	// The three newest survive; the oldest were evicted.
	if got[0].Ref != "j" || got[2].Ref != "h" {
		t.Errorf("survivors = %v, want the newest three", []string{got[0].Ref, got[1].Ref, got[2].Ref})
	}
}

func TestListLimit(t *testing.T) {
	l := openTemp(t, 1000)
	for i := 0; i < 50; i++ {
		_ = l.Append(Event{Type: JobAdmitted, Ref: "r"})
	}
	got, _ := l.List(Filter{Limit: 10})
	if len(got) != 10 {
		t.Errorf("limit 10 returned %d", len(got))
	}
	got, _ = l.List(Filter{}) // default cap 100 > 50
	if len(got) != 50 {
		t.Errorf("default returned %d, want all 50", len(got))
	}
}

func TestSinceFilter(t *testing.T) {
	l := openTemp(t, 100)
	cutoff := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	_ = l.Append(Event{Type: JobAdmitted, Ref: "old", DateCreated: cutoff.Add(-time.Hour)})
	_ = l.Append(Event{Type: JobAdmitted, Ref: "new", DateCreated: cutoff.Add(time.Hour)})
	got, _ := l.List(Filter{Since: cutoff})
	if len(got) != 1 || got[0].Ref != "new" {
		t.Errorf("since filter = %+v, want only the newer event", got)
	}
}

func TestRecorderNilSafe(t *testing.T) {
	var r *Recorder
	r.JobAdmitted("id", "a", "b", "c", "d") // must not panic
	r = NewRecorder(nil, nil)
	r.GCApplied("eng", 1, 2, 3, 0) // nil log, no panic
}

func TestRecorderEmits(t *testing.T) {
	l := openTemp(t, 100)
	r := NewRecorder(l, nil)
	r.JobAdmitted("job-1", "a:1", "up", "cache", "sha256:x")
	r.GCApplied("eng", 2, 1, 1, 0)
	r.Pinned("eng", "*:stable", false)
	r.ImagePulled("eng", "b:1")

	got, _ := l.List(Filter{})
	if len(got) != 4 {
		t.Fatalf("emitted %d events, want 4", len(got))
	}
	byType := map[Type]Event{}
	for _, e := range got {
		byType[e.Type] = e
	}
	if e := byType[JobAdmitted]; e.Ref != "a:1" || e.Store != "cache" || e.Digest != "sha256:x" {
		t.Errorf("admitted = %+v", e)
	}
	// The admitted event must carry the job id so it can be correlated with the
	// job_done event by id from the durable log alone.
	if e := byType[JobAdmitted]; !strings.Contains(string(e.Detail), `"job":"job-1"`) {
		t.Errorf("admitted detail missing job id: %s", e.Detail)
	}
	if _, ok := byType[GCApplied]; !ok {
		t.Error("gc_applied not recorded")
	}
	if e := byType[Pinned]; e.Ref != "*:stable" {
		t.Errorf("pinned = %+v", e)
	}
}

// A cap DECREASE across restart forces one Append to evict a backlog; the ring
// bound must hold (the buggy cursor-delete kept ~every other straggler).
func TestRingEvictionBacklog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evt.db")
	big, err := Open(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := big.Append(Event{Type: JobAdmitted, Ref: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	big.Close()

	small, err := Open(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	if err := small.Append(Event{Type: JobAdmitted, Ref: "newest"}); err != nil {
		t.Fatal(err)
	}
	got, err := small.List(Filter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("after cap decrease + append, kept %d entries, want cap 3", len(got))
	}
	if got[0].Ref != "newest" {
		t.Errorf("newest survivor = %q, want the just-appended entry", got[0].Ref)
	}
}

func TestRecorderImageRemoved(t *testing.T) {
	l := openTemp(t, 100)
	NewRecorder(l, nil).ImageRemoved("eng", "cache.local/a:1", "sha256:abc", "age_exceeded")
	got, _ := l.List(Filter{Type: ImageRemove})
	if len(got) != 1 || got[0].Ref != "cache.local/a:1" || got[0].Store != "eng" || got[0].Digest != "sha256:abc" {
		t.Errorf("image_removed = %+v", got)
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(got[0].Detail, &d) != nil || d.Reason != "age_exceeded" {
		t.Errorf("image_removed reason = %q, want age_exceeded (detail %s)", d.Reason, got[0].Detail)
	}
}

// A fallback is recorded durably: which store could not serve the job, which
// one was tried instead, and why — correlated to the job by id, like the rest
// of a job's lifecycle.
func TestRecorderJobFellBack(t *testing.T) {
	l := openTemp(t, 16)
	r := NewRecorder(l, nil)
	r.JobFellBack("job-9", "cr.example.com/app:1", "cache", "origin", "manifest unknown")

	got, err := l.List(Filter{Type: JobFallback})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	e := got[0]
	if e.Ref != "cr.example.com/app:1" || e.Store != "origin" || e.Error != "manifest unknown" {
		t.Errorf("event = %+v", e)
	}
	var d struct {
		Job    string `json:"job"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(e.Detail, &d); err != nil {
		t.Fatal(err)
	}
	if d.Job != "job-9" || d.Source != "cache" {
		t.Errorf("detail = %+v, want the job id and the source that failed", d)
	}
}
