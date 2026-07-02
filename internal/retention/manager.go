package retention

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

	now    func() time.Time
	signal chan struct{}
	onRun  func(Decision) // test hook

	// mu guards the scheduler/watcher state read by Status/Watcher from the API.
	mu       sync.Mutex
	started  time.Time
	lastRun  time.Time
	wakeAt   time.Time
	running  bool
	watchers map[string]*WatcherStatus
}

func NewManager(ix *Index, engines map[string]down.Engine, policy Policy, sched Schedule) *Manager {
	return &Manager{
		ix: ix, engines: engines, policy: policy, sched: sched,
		now: time.Now, signal: make(chan struct{}, 1),
		watchers: make(map[string]*WatcherStatus, len(engines)),
	}
}

// Status is a snapshot of the GC scheduler: the otherwise-invisible answers to
// "when will GC next run, under what policy, and why is it holding off".
type Status struct {
	Enabled bool `json:"enabled"` // scheduler running (interval > 0)
	Running bool `json:"running"` // a GC pass is executing right now

	Started    time.Time `json:"started,omitzero"`
	LastRun    time.Time `json:"last_run,omitzero"`
	NextWake   time.Time `json:"next_wake,omitzero"`
	GraceUntil time.Time `json:"grace_until,omitzero"` // age deletion held off until then

	Policy   PolicyStatus           `json:"policy"`
	Schedule ScheduleStatus         `json:"schedule"`
	Stores   map[string]StoreCounts `json:"stores"` // per-engine index sizes
}

type PolicyStatus struct {
	MaxAge string   `json:"max_age"`
	KeepN  int      `json:"keep_n"`
	Pins   []string `json:"pins,omitempty"`
}

type ScheduleStatus struct {
	Interval    string `json:"interval"`
	MinInterval string `json:"min_interval"`
	Grace       string `json:"grace"`
}

type StoreCounts struct {
	Records int `json:"records"`
	Pins    int `json:"pins"`
}

// Status snapshots the scheduler state and cheap per-engine index counts. It
// never probes live daemons.
func (m *Manager) Status() Status {
	m.mu.Lock()
	st := Status{
		Enabled:    m.sched.Interval > 0,
		Running:    m.running,
		Started:    m.started,
		LastRun:    m.lastRun,
		NextWake:   m.wakeAt,
		GraceUntil: m.graceUntilLocked(),
	}
	m.mu.Unlock()
	st.Policy = PolicyStatus{MaxAge: m.policy.MaxAge.String(), KeepN: m.policy.KeepN, Pins: m.policy.Pins}
	st.Schedule = ScheduleStatus{
		Interval:    m.sched.Interval.String(),
		MinInterval: m.sched.MinInterval.String(),
		Grace:       m.sched.Grace.String(),
	}
	st.Stores = make(map[string]StoreCounts, len(m.engines))
	for name := range m.engines {
		records, pins, err := m.ix.Counts(name)
		if err != nil {
			continue
		}
		st.Stores[name] = StoreCounts{Records: records, Pins: pins}
	}
	return st
}

// WatcherStatus is one engine's usage-watcher health. A dead event stream
// freezes LastUsed and silently degrades age GC; alert on connected=false or a
// stale last_event_at.
type WatcherStatus struct {
	Connected   bool      `json:"connected"`
	Since       time.Time `json:"watching_since,omitzero"`
	LastEventAt time.Time `json:"last_event_at,omitzero"` // receipt time of the last usage event
	LastSeedAt  time.Time `json:"last_seed_at,omitzero"`
	Reconnects  int64     `json:"reconnects"` // times the event stream ended and was re-established
	LastError   string    `json:"last_error,omitempty"`
}

// Watcher reports an engine's usage-watcher status; ok is false for a store
// with no retention watcher.
func (m *Manager) Watcher(engine string) (WatcherStatus, bool) {
	if _, tracked := m.engines[engine]; !tracked {
		return WatcherStatus{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws := m.watchers[engine]; ws != nil {
		return *ws, true
	}
	return WatcherStatus{}, true // engine tracked, watcher not started yet
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

func (m *Manager) Pin(engine, ref string, pattern bool) error { return m.ix.Pin(engine, ref, pattern) }
func (m *Manager) Unpin(engine, ref string) error             { return m.ix.Unpin(engine, ref) }
func (m *Manager) Pins(engine string) ([]PinEntry, error)     { return m.ix.Pins(engine) }

// --- usage watchers ---

// StartWatchers launches one usage watcher per engine: cold-start seed, then a
// reconnecting WatchUsage loop that stamps the index.
func (m *Manager) StartWatchers(ctx context.Context) {
	m.registerGauges(ctx)
	for name, eng := range m.engines {
		go m.watch(ctx, name, eng)
	}
}

// registerGauges observes per-engine retention index sizes (records and pins).
func (m *Manager) registerGauges(ctx context.Context) {
	mt := otx.Meter(ctx)
	records, _ := mt.Int64ObservableGauge("gantry.retention.records",
		metric.WithDescription("retention index records per engine store"))
	pins, _ := mt.Int64ObservableGauge("gantry.retention.pins",
		metric.WithDescription("pinned references per engine store"))
	_, _ = mt.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for name := range m.engines {
			nrec, npin, err := m.ix.Counts(name)
			if err != nil {
				continue
			}
			store_attr := metric.WithAttributes(attribute.String("store", name))
			o.ObserveInt64(records, int64(nrec), store_attr)
			o.ObserveInt64(pins, int64(npin), store_attr)
		}
		return nil
	}, records, pins)
}

func (m *Manager) watch(ctx context.Context, name string, eng down.Engine) {
	ws := &WatcherStatus{}
	m.mu.Lock()
	m.watchers[name] = ws
	m.mu.Unlock()
	seed := func() bool {
		err := eng.SeedUsage(ctx, func(ref string, at time.Time) { _ = m.ix.Seed(name, ref, at) })
		m.mu.Lock()
		ws.LastSeedAt = m.now()
		if err != nil {
			ws.LastError = err.Error()
		}
		m.mu.Unlock()
		return err == nil
	}
	log.From(ctx).Info("usage watcher started", slog.String("engine", name))
	reachable := seed()
	for ctx.Err() == nil {
		// Only claim connected when the preceding seed proved the daemon is
		// reachable — an optimistic stamp would report a healthy stream while
		// WatchUsage fails in a tight reconnect loop.
		if reachable {
			m.mu.Lock()
			ws.Connected = true
			if ws.Since.IsZero() {
				ws.Since = m.now()
			}
			m.mu.Unlock()
		}
		err := eng.WatchUsage(ctx, func(ref string, at time.Time) {
			_ = m.ix.Touch(name, ref, at)
			m.mu.Lock()
			ws.LastEventAt = m.now()
			m.mu.Unlock()
			m.poke()
		})
		m.mu.Lock()
		ws.Connected = false
		ws.Reconnects++
		if err != nil {
			ws.LastError = err.Error()
		}
		m.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		log.From(ctx).Debug("usage watcher reconnecting", slog.String("engine", name))
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second): // backoff, then re-seed to catch the gap
		}
		reachable = seed()
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
		_, _ = m.ix.Delete(name, c.Ref)
	}
	if len(res.Deleted)+len(res.Untagged) > 0 {
		log.From(ctx).Info("gc collected", slog.String("store", name),
			slog.Int("deleted", len(res.Deleted)), slog.Int("untagged", len(res.Untagged)), slog.Int("evaluated", res.Evaluated))
	}
	if len(res.Errors) > 0 {
		log.From(ctx).Warn("gc removal errors", slog.String("store", name), slog.Int("count", len(res.Errors)))
	}
	return res
}

func (m *Manager) graceUntil() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.graceUntilLocked()
}

func (m *Manager) graceUntilLocked() time.Time {
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
	m.mu.Lock()
	m.started = m.now()
	m.mu.Unlock()
	for {
		m.mu.Lock()
		m.running = true
		m.mu.Unlock()
		dec := m.gcAll(ctx)
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		if m.onRun != nil {
			m.onRun(dec)
		}
		d := m.nextWake(dec)
		m.mu.Lock()
		m.wakeAt = m.now().Add(d)
		m.mu.Unlock()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-m.signal:
			timer.Stop()
			d := m.sched.MinInterval - m.now().Sub(m.lastRunAt())
			if d < 0 {
				d = 0
			}
			// The poke short-circuits the armed timer; keep next_wake honest.
			m.mu.Lock()
			m.wakeAt = m.now().Add(d)
			m.mu.Unlock()
			if d > 0 {
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
			log.From(ctx).Warn("gc plan failed", slog.String("store", name), slog.String("error", err.Error()))
			continue
		}
		m.apply(ctx, name, eng, dec)
		merged.Delete = append(merged.Delete, dec.Delete...)
		merged.Keep = append(merged.Keep, dec.Keep...)
		if !dec.NextAgeOut.IsZero() && (merged.NextAgeOut.IsZero() || dec.NextAgeOut.Before(merged.NextAgeOut)) {
			merged.NextAgeOut = dec.NextAgeOut
		}
	}
	m.mu.Lock()
	m.lastRun = m.now()
	m.mu.Unlock()
	return merged
}

func (m *Manager) lastRunAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRun
}

func (m *Manager) nextWake(dec Decision) time.Duration {
	cap := m.sched.Interval
	next := dec.NextAgeOut
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
