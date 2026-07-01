package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/lesomnus/gantry/internal/down"
)

// Schedule controls the adaptive GC scheduler.
type Schedule struct {
	Interval    time.Duration // safety/idle cap on sleep between GC checks
	MinInterval time.Duration // debounce floor between GC runs
	Grace       time.Duration // hold off age-deletion this long after start
}

// ApplyResult reports a GC apply outcome.
type ApplyResult struct {
	Deleted   []string `json:"deleted"`          // content-hash IDs whose bytes were freed
	Untagged  []string `json:"untagged"`         // refs whose tag was removed but content may remain
	Errors    []string `json:"errors,omitempty"` // per-ref removal failures, "<ref>: <err>"
	Evaluated int      `json:"evaluated"`        // number of records considered (delete+keep)
}

// Manager ties the index, the engines, the usage watchers, and the adaptive GC
// scheduler together.
type Manager struct {
	ix      *Index
	engines map[string]down.Engine
	policy  Policy
	sched   Schedule

	now     func() time.Time
	signal  chan struct{}
	started time.Time
	lastRun time.Time
	onRun   func(Decision) // test hook
}

func NewManager(ix *Index, engines map[string]down.Engine, policy Policy, sched Schedule) *Manager {
	return &Manager{
		ix: ix, engines: engines, policy: policy, sched: sched,
		now: time.Now, signal: make(chan struct{}, 1),
	}
}

func (m *Manager) Index() *Index { return m.ix }

// DefaultPolicy is the configured policy used by the scheduler and as the API default.
func (m *Manager) DefaultPolicy() Policy { return m.policy }

func (m *Manager) poke() {
	select {
	case m.signal <- struct{}{}:
	default:
	}
}

// Distributed records a gantry push to an engine (a usage fallback signal).
func (m *Manager) Distributed(engine, ref string, t time.Time) {
	_ = m.ix.Distributed(engine, ref, t)
	m.poke()
}

func (m *Manager) Pin(engine, ref string) error         { return m.ix.Pin(engine, ref) }
func (m *Manager) Unpin(engine, ref string) error       { return m.ix.Unpin(engine, ref) }
func (m *Manager) Pins(engine string) ([]string, error) { return m.ix.Pins(engine) }

// --- usage watchers ---

// StartWatchers launches one usage watcher per engine: cold-start seed, then a
// reconnecting WatchUsage loop that stamps the index.
func (m *Manager) StartWatchers(ctx context.Context) {
	for name, eng := range m.engines {
		go m.watch(ctx, name, eng)
	}
}

func (m *Manager) watch(ctx context.Context, name string, eng down.Engine) {
	seed := func() {
		_ = eng.SeedUsage(ctx, func(ref string, at time.Time) { _ = m.ix.Seed(name, ref, at) })
	}
	seed()
	for ctx.Err() == nil {
		_ = eng.WatchUsage(ctx, func(ref string, at time.Time) {
			_ = m.ix.Touch(name, ref, at)
			m.poke()
		})
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second): // backoff, then re-seed to catch the gap
		}
		seed()
	}
}

// --- GC ---

// Plan evaluates the policy for one engine without deleting anything.
func (m *Manager) Plan(ctx context.Context, engine string, p Policy) (Decision, error) {
	eng, ok := m.engines[engine]
	if !ok {
		return Decision{}, fmt.Errorf("store %q has no retention", engine)
	}
	return m.plan(ctx, engine, eng, p)
}

func (m *Manager) plan(ctx context.Context, name string, eng down.Engine, p Policy) (Decision, error) {
	recs, err := m.ix.List(name)
	if err != nil {
		return Decision{}, err
	}
	inUse, err := eng.InUse(ctx)
	if err != nil {
		return Decision{}, err
	}
	return Evaluate(m.now(), recs, inUse, p, m.graceUntil()), nil
}

// Apply executes the deletions in a decision and syncs the index.
func (m *Manager) Apply(ctx context.Context, engine string, dec Decision) (ApplyResult, error) {
	eng, ok := m.engines[engine]
	if !ok {
		return ApplyResult{}, fmt.Errorf("store %q has no retention", engine)
	}
	return m.apply(ctx, engine, eng, dec), nil
}

func (m *Manager) apply(ctx context.Context, name string, eng down.Engine, dec Decision) ApplyResult {
	res := ApplyResult{Evaluated: len(dec.Delete) + len(dec.Keep)}
	for _, c := range dec.Delete {
		rr, err := eng.Remove(ctx, c.Ref)
		if err != nil {
			res.Errors = append(res.Errors, c.Ref+": "+err.Error())
			continue
		}
		res.Deleted = append(res.Deleted, rr.Deleted...)
		res.Untagged = append(res.Untagged, rr.Untagged...)
		_ = m.ix.Delete(name, c.Ref)
	}
	return res
}

func (m *Manager) graceUntil() time.Time {
	if m.started.IsZero() || m.sched.Grace <= 0 {
		return time.Time{}
	}
	return m.started.Add(m.sched.Grace)
}

// --- adaptive scheduler ---

// StartScheduler runs GC and then sleeps until the soonest record could age out
// (capped at Interval), waking early when a usage/distribute event arrives. With
// no deletable-by-age records it idles at Interval — it does not busy-tick.
func (m *Manager) StartScheduler(ctx context.Context) {
	if m.sched.Interval <= 0 {
		return // scheduling disabled; manual GC only
	}
	m.started = m.now()
	for {
		dec := m.gcAll(ctx)
		if m.onRun != nil {
			m.onRun(dec)
		}
		timer := time.NewTimer(m.nextWake(dec))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-m.signal:
			timer.Stop()
			if d := m.sched.MinInterval - m.now().Sub(m.lastRun); d > 0 {
				deb := time.NewTimer(d)
				select {
				case <-ctx.Done():
					deb.Stop()
					return
				case <-deb.C:
				}
			}
		}
	}
}

func (m *Manager) gcAll(ctx context.Context) Decision {
	var merged Decision
	for name, eng := range m.engines {
		dec, err := m.plan(ctx, name, eng, m.policy)
		if err != nil {
			continue
		}
		m.apply(ctx, name, eng, dec)
		merged.Delete = append(merged.Delete, dec.Delete...)
		merged.Keep = append(merged.Keep, dec.Keep...)
		if !dec.earliestAgeOut.IsZero() && (merged.earliestAgeOut.IsZero() || dec.earliestAgeOut.Before(merged.earliestAgeOut)) {
			merged.earliestAgeOut = dec.earliestAgeOut
		}
	}
	m.lastRun = m.now()
	return merged
}

func (m *Manager) nextWake(dec Decision) time.Duration {
	cap := m.sched.Interval
	next := dec.NextAgeOut()
	if next.IsZero() {
		return cap // nothing aging -> idle until Interval (or an event pokes)
	}
	d := next.Sub(m.now())
	if d < m.sched.MinInterval {
		d = m.sched.MinInterval
	}
	if d > cap {
		d = cap
	}
	return d
}
