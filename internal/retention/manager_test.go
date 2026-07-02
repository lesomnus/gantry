package retention

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/gantry/internal/down"
)

type fakeEng struct {
	name  string
	mu    sync.Mutex
	inUse map[string]bool
	rmed  []string
	watch func(context.Context, down.UsageSink) error
}

func (f *fakeEng) Name() string                                          { return f.name }
func (f *fakeEng) Kind() string                                          { return "docker" }
func (f *fakeEng) Ready(context.Context) error                           { return nil }
func (f *fakeEng) Pull(context.Context, string, string, down.Sink) error { return nil }
func (f *fakeEng) InUse(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inUse, nil
}
func (f *fakeEng) SeedUsage(context.Context, down.UsageSink) error { return nil }
func (f *fakeEng) WatchUsage(ctx context.Context, sink down.UsageSink) error {
	if f.watch != nil {
		return f.watch(ctx, sink)
	}
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeEng) Remove(_ context.Context, ref string) (down.RemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rmed = append(f.rmed, ref)
	return down.RemoveResult{Deleted: []string{ref}}, nil
}
func (f *fakeEng) Close() error { return nil }
func (f *fakeEng) removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rmed...)
}

func TestManagerPlanApply(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("d", "r/a:old", time.Now().Add(-100*time.Hour))
	_ = ix.Touch("d", "r/a:recent", time.Now())
	eng := &fakeEng{name: "d"}
	m := NewManager(ix, map[string]down.Engine{"d": eng}, Policy{MaxAge: time.Hour}, Schedule{})

	dec, err := m.Plan(context.Background(), "d", m.policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.Delete) != 1 || dec.Delete[0].Ref != "r/a:old" {
		t.Fatalf("plan delete = %+v", dec.Delete)
	}
	res, err := m.Apply(context.Background(), "d", dec)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != "r/a:old" {
		t.Errorf("apply = %+v", res)
	}
	if got := eng.removed(); len(got) != 1 || got[0] != "r/a:old" {
		t.Errorf("engine removed = %v", got)
	}
	if recs, _ := ix.List("d"); len(recs) != 1 || recs[0].Ref != "r/a:recent" {
		t.Errorf("index after apply = %+v", recs)
	}
}

func TestSchedulerIdlesAndWakesOnEvent(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("d", "r/a:1", time.Now()) // recent: won't age out for an hour
	eng := &fakeEng{name: "d"}
	m := NewManager(ix, map[string]down.Engine{"d": eng}, Policy{MaxAge: time.Hour},
		Schedule{Interval: 10 * time.Second, MinInterval: time.Millisecond})
	var runs int32
	m.onRun = func(Decision) { atomic.AddInt32(&runs, 1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.StartScheduler(ctx)

	time.Sleep(120 * time.Millisecond)
	if n := atomic.LoadInt32(&runs); n != 1 {
		t.Errorf("with a 10s cap and nothing aging, expected exactly 1 run, got %d (busy-ticking?)", n)
	}
	m.Distributed("d", "r/a:1", time.Now()) // pokes the scheduler
	time.Sleep(120 * time.Millisecond)
	if n := atomic.LoadInt32(&runs); n < 2 {
		t.Errorf("event should wake the scheduler, got %d runs", n)
	}
}

func TestSchedulerWakesAtAgeDeadline(t *testing.T) {
	ix := openTemp(t)
	// ages out ~80ms after start; cap is large so the wake must be deadline-driven.
	_ = ix.Touch("d", "r/a:1", time.Now().Add(-(200*time.Millisecond - 80*time.Millisecond)))
	eng := &fakeEng{name: "d"}
	m := NewManager(ix, map[string]down.Engine{"d": eng}, Policy{MaxAge: 200 * time.Millisecond},
		Schedule{Interval: 10 * time.Second, MinInterval: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.StartScheduler(ctx)

	time.Sleep(40 * time.Millisecond)
	if got := eng.removed(); len(got) != 0 {
		t.Errorf("deleted too early: %v", got)
	}
	time.Sleep(200 * time.Millisecond) // past the age deadline
	if got := eng.removed(); len(got) != 1 || got[0] != "r/a:1" {
		t.Errorf("scheduler did not wake at the age deadline: removed=%v", got)
	}
}

func TestWatcherStampsIndex(t *testing.T) {
	ix := openTemp(t)
	ts := time.Now()
	eng := &fakeEng{name: "d", watch: func(ctx context.Context, sink down.UsageSink) error {
		sink("r/a:used", ts)
		<-ctx.Done()
		return ctx.Err()
	}}
	m := NewManager(ix, map[string]down.Engine{"d": eng}, Policy{}, Schedule{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartWatchers(ctx)
	time.Sleep(80 * time.Millisecond)
	recs, _ := ix.List("d")
	if len(recs) != 1 || recs[0].Ref != "r/a:used" || !recs[0].LastUsed.Equal(ts) {
		t.Errorf("watcher did not stamp index: %+v", recs)
	}
}

func TestWatcherStatusStamps(t *testing.T) {
	ix := openTemp(t)
	first := true
	eng := &fakeEng{name: "d", watch: func(ctx context.Context, sink down.UsageSink) error {
		if first {
			first = false
			sink("cache.local/a/app:1", time.Now())
			return errors.New("stream dropped") // watcher must reconnect and record the error
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	m := NewManager(ix, map[string]down.Engine{"d": eng}, Policy{}, Schedule{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.StartWatchers(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ws, ok := m.Watcher("d")
		if !ok {
			t.Fatal("engine must be tracked")
		}
		if !ws.LastEventAt.IsZero() && ws.Reconnects >= 1 && ws.LastError == "stream dropped" && !ws.LastSeedAt.IsZero() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	ws, _ := m.Watcher("d")
	t.Fatalf("watcher status not stamped in time: %+v", ws)
}

func TestManagerStatusSchedulerFields(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("d", "r/a:1", time.Now())
	eng := &fakeEng{name: "d"}
	m := NewManager(ix, map[string]down.Engine{"d": eng}, Policy{MaxAge: time.Hour},
		Schedule{Interval: time.Hour, MinInterval: time.Minute, Grace: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.StartScheduler(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status()
		if !st.LastRun.IsZero() && !st.NextWake.IsZero() {
			if !st.Enabled {
				t.Error("enabled must be true with a positive interval")
			}
			if st.GraceUntil.IsZero() {
				t.Error("grace_until must be set after start")
			}
			if st.Schedule.Interval != "1h0m0s" {
				t.Errorf("schedule.interval = %q", st.Schedule.Interval)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("scheduler status not populated in time: %+v", m.Status())
}

// recRecorder captures emitted audit calls.
type recRecorder struct {
	gc      int
	removed []string
	pins    []string
}

func (r *recRecorder) GCApplied(string, int, int, int) { r.gc++ }
func (r *recRecorder) ImageRemoved(_, ref string)      { r.removed = append(r.removed, ref) }
func (r *recRecorder) Pinned(_, value string, _ bool)  { r.pins = append(r.pins, value) }

func TestApplyEmitsAuditEvents(t *testing.T) {
	ix := openTemp(t)
	_ = ix.Touch("d", "r/a:old", time.Now().Add(-100*time.Hour))
	eng := &fakeEng{name: "d"}
	m := NewManager(ix, map[string]down.Engine{"d": eng}, Policy{MaxAge: time.Hour}, Schedule{})
	rec := &recRecorder{}
	m.SetRecorder(rec)

	dec, err := m.Plan(context.Background(), "d", m.policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(context.Background(), "d", dec); err != nil {
		t.Fatal(err)
	}
	if rec.gc != 1 {
		t.Errorf("gc_applied emitted %d times, want 1", rec.gc)
	}
	if len(rec.removed) != 1 || rec.removed[0] != "r/a:old" {
		t.Errorf("image_removed = %v, want [r/a:old]", rec.removed)
	}
	if err := m.Pin("d", "r/a:new", false); err != nil {
		t.Fatal(err)
	}
	if len(rec.pins) != 1 || rec.pins[0] != "r/a:new" {
		t.Errorf("pinned = %v", rec.pins)
	}
}
