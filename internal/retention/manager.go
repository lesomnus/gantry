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

// Schedule controls one store's adaptive GC scheduler.
type Schedule struct {
	Interval    time.Duration // safety/idle cap on sleep between GC checks
	MinInterval time.Duration // debounce floor between GC runs
	Grace       time.Duration // hold off deletion this long after start
}

// ApplyResult reports a GC apply outcome.
type ApplyResult struct {
	Deleted   []string `json:"deleted"`           // content-hash IDs whose bytes were freed
	Untagged  []string `json:"untagged"`          // refs whose tag was removed but content may remain
	Reaped    []string `json:"reaped,omitempty"`  // untagged image IDs whose content was reaped
	Skipped   []string `json:"skipped,omitempty"` // untagged image IDs not reapable right now (re-tagged, container ref, in-flight pull)
	Errors    []string `json:"errors,omitempty"`  // per-ref removal failures, "<ref>: <err>"
	Evaluated int      `json:"evaluated"`         // number of records considered (delete+keep)
}

// Store describes one engine store's retention setup, passed to NewManager. Each
// store owns its own usage index, per-repo rules, and scheduler cadence.
type Store struct {
	Name     string
	Engine   down.Engine
	Index    *Index
	Rules    []Rule
	Schedule Schedule
	// UntaggedAfter reaps an image this long after it was first observed with no
	// tags (docker engines only — requires the down.Reconciler capability). Zero
	// disables the reaper; the inventory scan itself still runs when the engine
	// supports it, seeding unknown tagged refs into the index.
	UntaggedAfter time.Duration
}

// Manager runs per-store retention: every store has its own index, rules, grace
// window, adaptive scheduler, and usage watcher — fully independent of the
// others. There is no global retention policy.
type Manager struct {
	units map[string]*unit

	now   func() time.Time
	rec   Recorder       // audit log (nil = disabled)
	onRun func(Decision) // test hook, fired after each store's GC pass
}

// unit is one engine store's retention state.
type unit struct {
	m      *Manager
	name   string
	engine down.Engine
	ix     *Index
	rules  []Rule
	sched  Schedule

	// recon is the engine's optional inventory-scan capability (nil when the
	// engine kind has none, e.g. containerd); untaggedAfter is the configured
	// reap delay (zero = reaper off).
	recon         down.Reconciler
	untaggedAfter time.Duration

	signal chan struct{}

	mu      sync.Mutex
	started time.Time
	lastRun time.Time
	wakeAt  time.Time
	running bool
	watcher WatcherStatus
}

func NewManager(stores []Store) *Manager {
	m := &Manager{units: make(map[string]*unit, len(stores)), now: time.Now}
	for _, s := range stores {
		recon, _ := s.Engine.(down.Reconciler)
		m.units[s.Name] = &unit{
			m: m, name: s.Name, engine: s.Engine, ix: s.Index,
			rules: s.Rules, sched: s.Schedule, signal: make(chan struct{}, 1),
			recon: recon, untaggedAfter: s.UntaggedAfter,
		}
	}
	return m
}

// Recorder receives GC-apply and manual pin/remove audit events.
type Recorder interface {
	GCApplied(store string, deleted, untagged, reaped, errs int)
	ImageRemoved(store, ref string)
	Pinned(store, value string, unpin bool)
}

// SetRecorder wires the audit log. Set before Start.
func (m *Manager) SetRecorder(r Recorder) { m.rec = r }

// Close releases every store's index. Call at shutdown.
func (m *Manager) Close() error {
	var err error
	for _, u := range m.units {
		if e := u.ix.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// --- status ---

// Status is a snapshot of every store's GC scheduler.
type Status struct {
	Enabled bool                   `json:"enabled"` // at least one store has scheduling on
	Stores  map[string]StoreStatus `json:"stores"`
}

// StoreStatus is one store's scheduler state, rules, and index counts.
type StoreStatus struct {
	Running       bool           `json:"running"`
	Started       time.Time      `json:"started,omitzero"`
	LastRun       time.Time      `json:"last_run,omitzero"`
	NextWake      time.Time      `json:"next_wake,omitzero"`
	GraceUntil    time.Time      `json:"grace_until,omitzero"`
	Schedule      ScheduleStatus `json:"schedule"`
	Rules         []RuleStatus   `json:"rules"`
	Records       int            `json:"records"`
	Pins          int            `json:"pins"`
	Untagged      int            `json:"untagged"`                 // tracked untagged images (reap clocks running)
	UntaggedAfter string         `json:"untagged_after,omitempty"` // reap delay; absent when the reaper is off
}

type ScheduleStatus struct {
	Interval    string `json:"interval"`
	MinInterval string `json:"min_interval"`
	Grace       string `json:"grace"`
}

// RuleStatus mirrors a per-repo rule for the API; unset fields are omitted.
type RuleStatus struct {
	Repo    string   `json:"repo"`
	MaxAge  string   `json:"max_age,omitempty"`
	KeepN   *int     `json:"keep_n,omitempty"`
	MaxN    *int     `json:"max_n,omitempty"`
	MaxIdle string   `json:"max_idle,omitempty"`
	Pins    []string `json:"pins,omitempty"`
}

// Status snapshots each store's scheduler state and cheap index counts. It never
// probes live daemons.
func (m *Manager) Status() Status {
	st := Status{Stores: make(map[string]StoreStatus, len(m.units))}
	for name, u := range m.units {
		u.mu.Lock()
		ss := StoreStatus{
			Running:    u.running,
			Started:    u.started,
			LastRun:    u.lastRun,
			NextWake:   u.wakeAt,
			GraceUntil: u.graceUntilLocked(),
		}
		u.mu.Unlock()
		if u.sched.Interval > 0 {
			st.Enabled = true
		}
		ss.Schedule = ScheduleStatus{
			Interval:    u.sched.Interval.String(),
			MinInterval: u.sched.MinInterval.String(),
			Grace:       u.sched.Grace.String(),
		}
		ss.Rules = ruleStatuses(u.rules)
		if nrec, npin, nunt, err := u.ix.Counts(name); err == nil {
			ss.Records, ss.Pins, ss.Untagged = nrec, npin, nunt
		}
		if u.untaggedAfter > 0 {
			ss.UntaggedAfter = u.untaggedAfter.String()
		}
		st.Stores[name] = ss
	}
	return st
}

func ruleStatuses(rules []Rule) []RuleStatus {
	out := make([]RuleStatus, 0, len(rules))
	for _, r := range rules {
		rs := RuleStatus{Repo: r.Repo, KeepN: r.KeepN, MaxN: r.MaxN, Pins: r.Pins}
		if r.MaxAge != nil {
			rs.MaxAge = r.MaxAge.String()
		}
		if r.MaxIdle != nil {
			rs.MaxIdle = r.MaxIdle.String()
		}
		out = append(out, rs)
	}
	return out
}

// WatcherStatus is one engine's usage-watcher health. A dead event stream
// freezes DateLastUsed and silently degrades age GC; alert on connected=false or a
// stale date_last_event.
type WatcherStatus struct {
	Connected     bool      `json:"connected"`
	Since         time.Time `json:"watching_since,omitzero"`
	DateLastEvent time.Time `json:"date_last_event,omitzero"` // receipt time of the last usage event
	DateLastSeed  time.Time `json:"date_last_seed,omitzero"`
	Reconnects    int64     `json:"reconnects"` // times the event stream ended and was re-established
	LastError     string    `json:"last_error,omitempty"`
}

// Watcher reports an engine's usage-watcher status; ok is false for a store
// with no retention.
func (m *Manager) Watcher(engine string) (WatcherStatus, bool) {
	u, ok := m.units[engine]
	if !ok {
		return WatcherStatus{}, false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.watcher, true
}

// --- index dispatch (per store) ---

// List returns a store's retention records.
func (m *Manager) List(engine string) ([]Record, error) {
	u, ok := m.units[engine]
	if !ok {
		return nil, fmt.Errorf("store %q has no retention", engine)
	}
	return u.ix.List(engine)
}

// DeleteRecord removes one record from a store's index (does not touch the
// engine). A ref that is a tracked untagged image ID purges that entry instead.
func (m *Manager) DeleteRecord(engine, ref string) (bool, error) {
	u, ok := m.units[engine]
	if !ok {
		return false, fmt.Errorf("store %q has no retention", engine)
	}
	existed, err := u.ix.Delete(engine, ref)
	if err != nil || existed {
		return existed, err
	}
	return u.ix.DeleteUntagged(engine, ref)
}

// ListUntagged returns a store's tracked untagged images (their reap clocks).
func (m *Manager) ListUntagged(engine string) ([]UntaggedEntry, error) {
	u, ok := m.units[engine]
	if !ok {
		return nil, fmt.Errorf("store %q has no retention", engine)
	}
	return u.ix.UntaggedEntries(engine)
}

// Distributed records a gantry push to an engine (a usage fallback signal).
func (m *Manager) Distributed(engine, ref string, t time.Time) {
	if u, ok := m.units[engine]; ok {
		_ = u.ix.Distributed(engine, ref, t)
		u.poke()
	}
}

func (m *Manager) Pin(engine, ref string, pattern bool) error {
	u, ok := m.units[engine]
	if !ok {
		return fmt.Errorf("store %q has no retention", engine)
	}
	err := u.ix.Pin(engine, ref, pattern)
	if err == nil && m.rec != nil {
		m.rec.Pinned(engine, ref, false)
	}
	return err
}

func (m *Manager) Unpin(engine, ref string) error {
	u, ok := m.units[engine]
	if !ok {
		return fmt.Errorf("store %q has no retention", engine)
	}
	err := u.ix.Unpin(engine, ref)
	if err == nil && m.rec != nil {
		m.rec.Pinned(engine, ref, true)
	}
	return err
}

func (m *Manager) Pins(engine string) ([]PinEntry, error) {
	u, ok := m.units[engine]
	if !ok {
		return nil, fmt.Errorf("store %q has no retention", engine)
	}
	return u.ix.Pins(engine)
}

// --- usage watchers ---

// StartWatchers launches one usage watcher per store: cold-start seed, then a
// reconnecting WatchUsage loop that stamps the store's index.
func (m *Manager) StartWatchers(ctx context.Context) {
	m.registerGauges(ctx)
	for _, u := range m.units {
		go u.watch(ctx)
	}
}

// registerGauges observes per-store retention index sizes (records, pins, and
// tracked untagged images).
func (m *Manager) registerGauges(ctx context.Context) {
	mt := otx.Meter(ctx)
	records, _ := mt.Int64ObservableGauge("gantry.retention.records",
		metric.WithDescription("retention index records per engine store"))
	pins, _ := mt.Int64ObservableGauge("gantry.retention.pins",
		metric.WithDescription("pinned references per engine store"))
	untagged, _ := mt.Int64ObservableGauge("gantry.retention.untagged",
		metric.WithDescription("tracked untagged images per engine store"))
	_, _ = mt.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for name, u := range m.units {
			nrec, npin, nunt, err := u.ix.Counts(name)
			if err != nil {
				continue
			}
			store_attr := metric.WithAttributes(attribute.String("store", name))
			o.ObserveInt64(records, int64(nrec), store_attr)
			o.ObserveInt64(pins, int64(npin), store_attr)
			o.ObserveInt64(untagged, int64(nunt), store_attr)
		}
		return nil
	}, records, pins, untagged)
}

func (u *unit) watch(ctx context.Context) {
	name, eng := u.name, u.engine
	seed := func() bool {
		err := eng.SeedUsage(ctx, func(ref string, at time.Time) { _ = u.ix.Seed(name, ref, at) })
		u.mu.Lock()
		u.watcher.DateLastSeed = u.m.now()
		if err != nil {
			u.watcher.LastError = err.Error()
		}
		u.mu.Unlock()
		return err == nil
	}
	log.From(ctx).Info("usage watcher started", slog.String("engine", name))
	reachable := seed()
	for ctx.Err() == nil {
		if reachable {
			u.mu.Lock()
			u.watcher.Connected = true
			if u.watcher.Since.IsZero() {
				u.watcher.Since = u.m.now()
			}
			u.mu.Unlock()
		}
		err := eng.WatchUsage(ctx, func(ref string, at time.Time) {
			_ = u.ix.Touch(name, ref, at)
			u.mu.Lock()
			u.watcher.DateLastEvent = u.m.now()
			u.mu.Unlock()
			u.poke()
		})
		u.mu.Lock()
		u.watcher.Connected = false
		u.watcher.Reconnects++
		if err != nil {
			u.watcher.LastError = err.Error()
		}
		u.mu.Unlock()
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

// Plan evaluates a store's rules without deleting. A non-nil override replaces
// the configured per-repo rules with a single blanket policy for every repo (used
// by the /gc endpoint's optional body); the override's UntaggedAfter likewise
// replaces the configured reap delay — unset (zero) turns the reaper off for
// the call, consistent with the other override fields.
func (m *Manager) Plan(ctx context.Context, engine string, override *Policy) (Decision, error) {
	u, ok := m.units[engine]
	if !ok {
		return Decision{}, fmt.Errorf("store %q has no retention", engine)
	}
	rules := u.rules
	after := u.untaggedAfter
	if override != nil {
		if override.UntaggedAfter > 0 {
			if u.recon == nil {
				return Decision{}, fmt.Errorf("store %q does not support untagged reaping", engine)
			}
			if u.untaggedAfter <= 0 {
				// "0s" in the config means this store must never reap — e.g. another
				// store reaps the same daemon (the config validation only blesses
				// one reaper per daemon). An ad-hoc override cannot re-enable it.
				return Decision{}, fmt.Errorf("store %q has untagged reaping disabled (untagged_after \"0s\"); an override cannot enable it", engine)
			}
		}
		rules = blanketRules(*override)
		after = override.UntaggedAfter
	}
	return u.plan(ctx, rules, after, nil)
}

// Apply executes the deletions in a decision and syncs the index.
func (m *Manager) Apply(ctx context.Context, engine string, dec Decision) (ApplyResult, error) {
	u, ok := m.units[engine]
	if !ok {
		return ApplyResult{}, fmt.Errorf("store %q has no retention", engine)
	}
	return u.apply(ctx, dec), nil
}

// blanketRules wraps a Policy as a single catch-all rule applied to every repo.
func blanketRules(p Policy) []Rule {
	return []Rule{{Repo: "**", MaxAge: &p.MaxAge, KeepN: &p.KeepN, MaxN: &p.MaxN, MaxIdle: &p.MaxIdle, Pins: p.Pins}}
}

// plan evaluates the rules plus, when the reaper is on, the untagged axis. A
// non-nil inv reuses an inventory the caller already fetched (gcOnce scans once
// for both reconciliation and planning); nil fetches one read-only, so the
// HTTP dry-run reflects live daemon state without writing any reap clock.
func (u *unit) plan(ctx context.Context, rules []Rule, after time.Duration, inv *down.Inventory) (Decision, error) {
	recs, err := u.ix.List(u.name)
	if err != nil {
		return Decision{}, err
	}
	inUse, err := u.engine.InUse(ctx)
	if err != nil {
		return Decision{}, err
	}
	dec := Evaluate(u.m.now(), recs, inUse, rules, u.graceUntil())
	if after <= 0 || u.recon == nil {
		return dec, nil
	}
	if inv == nil {
		v, err := u.recon.Images(ctx)
		if err != nil {
			return Decision{}, err
		}
		inv = &v
	}
	entries, err := u.ix.UntaggedEntries(u.name)
	if err != nil {
		return Decision{}, err
	}
	firstSeen := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		firstSeen[e.ID] = e.DateFirstSeen
	}
	pins, err := u.ix.Pins(u.name)
	if err != nil {
		return Decision{}, err
	}
	// Rule pins join as patterns: matchPin tries exact equality first, so exact
	// refs written in rules keep protecting too.
	for _, r := range rules {
		for _, p := range r.Pins {
			pins = append(pins, PinEntry{Value: p, Pattern: true})
		}
	}
	EvaluateUntagged(u.m.now(), UntaggedInput{
		Images:    inv.Untagged,
		FirstSeen: firstSeen,
		Records:   recs,
		InUse:     inUse,
		Pins:      pins,
		After:     after,
	}, u.graceUntil(), &dec)
	return dec, nil
}

// reconcile snapshots the engine's image store and syncs the index: tagged refs
// gantry has never observed get a record (DateFirstSeen = now, so the configured
// rules eventually manage images a human pulled while gantry was away), newly
// untagged images start their reap clock, and tracked entries whose image
// regained a tag or vanished are dropped. Returns the inventory for the plan
// that follows, or nil when the engine has no scan capability or the scan
// failed (GC then proceeds on index records alone, like before).
func (u *unit) reconcile(ctx context.Context) *down.Inventory {
	if u.recon == nil {
		return nil
	}
	inv, err := u.recon.Images(ctx)
	if err != nil {
		log.From(ctx).Warn("inventory scan failed", slog.String("store", u.name), slog.String("error", err.Error()))
		return nil
	}
	now := u.m.now()
	for _, ref := range inv.Refs {
		_ = u.ix.Observe(u.name, ref, now)
	}
	live := make(map[string]bool, len(inv.Untagged))
	for _, img := range inv.Untagged {
		live[img.ID] = true
		_ = u.ix.ObserveUntagged(u.name, img.ID, now)
	}
	if entries, err := u.ix.UntaggedEntries(u.name); err == nil {
		for _, e := range entries {
			if !live[e.ID] {
				_, _ = u.ix.DeleteUntagged(u.name, e.ID)
			}
		}
	}
	return &inv
}

func (u *unit) apply(ctx context.Context, dec Decision) ApplyResult {
	res := ApplyResult{Evaluated: len(dec.Delete) + len(dec.Keep)}
	for _, c := range dec.Delete {
		if c.ImageID != "" {
			u.reapOne(ctx, c.ImageID, &res)
			continue
		}
		rr, err := u.engine.Remove(ctx, c.Ref)
		if err != nil {
			res.Errors = append(res.Errors, c.Ref+": "+err.Error())
			continue
		}
		res.Deleted = append(res.Deleted, rr.Deleted...)
		res.Untagged = append(res.Untagged, rr.Untagged...)
		_, _ = u.ix.Delete(u.name, c.Ref)
		if u.m.rec != nil {
			u.m.rec.ImageRemoved(u.name, c.Ref)
		}
	}
	if len(res.Deleted)+len(res.Untagged)+len(res.Reaped) > 0 {
		log.From(ctx).Info("gc collected", slog.String("store", u.name),
			slog.Int("deleted", len(res.Deleted)), slog.Int("untagged", len(res.Untagged)),
			slog.Int("reaped", len(res.Reaped)), slog.Int("evaluated", res.Evaluated))
		if u.m.rec != nil {
			u.m.rec.GCApplied(u.name, len(res.Deleted), len(res.Untagged), len(res.Reaped), len(res.Errors))
		}
	}
	if len(res.Errors) > 0 {
		log.From(ctx).Warn("gc removal errors", slog.String("store", u.name), slog.Int("count", len(res.Errors)))
	}
	return res
}

// reapOne executes one untagged-image reap. The engine re-checks reapability
// right before removing (re-tagged, container reference, in-flight pull,
// index-owned digest reference) and reports not-ok without error for a
// transient hold — the entry stays tracked and the next pass retries; the next
// scan drops it if the image was re-tagged.
func (u *unit) reapOne(ctx context.Context, id string, res *ApplyResult) {
	if u.recon == nil {
		return
	}
	// The decision may be stale: a DELETE /image purge of the reap clock between
	// plan and apply cancels the reap.
	if has, err := u.ix.HasUntagged(u.name, id); err != nil || !has {
		res.Skipped = append(res.Skipped, id)
		return
	}
	// Re-read digest ownership at apply time: a digest-`as` job finishing
	// between the plan and this reap names the content only through a
	// RepoDigest + an index record — invisible to the engine's RepoTags
	// re-check, so the veto has to come from the live index.
	owned := func(string) bool { return false }
	if recs, err := u.ix.List(u.name); err == nil {
		byDigest := make(map[string]bool, len(recs))
		for _, r := range recs {
			if r.Digest != "" && r.Tag == "" {
				byDigest[r.Repo+"@"+r.Digest] = true
			}
		}
		owned = func(ref string) bool {
			repo, _, dg := parseRef(ref)
			return dg != "" && byDigest[repo+"@"+dg]
		}
	}
	rr, ok, err := u.recon.ReapUntagged(ctx, id, owned)
	res.Deleted = append(res.Deleted, rr.Deleted...)
	res.Untagged = append(res.Untagged, rr.Untagged...)
	if err != nil {
		res.Errors = append(res.Errors, id+": "+err.Error())
		return
	}
	if !ok {
		res.Skipped = append(res.Skipped, id)
		return
	}
	_, _ = u.ix.DeleteUntagged(u.name, id)
	// An image already gone (removed out-of-band or by a concurrent reap)
	// converges the index but is not this pass's kill — don't report it as one.
	if len(rr.Deleted)+len(rr.Untagged) == 0 {
		return
	}
	res.Reaped = append(res.Reaped, id)
	if u.m.rec != nil {
		u.m.rec.ImageRemoved(u.name, id)
	}
}

func (u *unit) graceUntil() time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.graceUntilLocked()
}

func (u *unit) graceUntilLocked() time.Time {
	if u.started.IsZero() || u.sched.Grace <= 0 {
		return time.Time{}
	}
	return u.started.Add(u.sched.Grace)
}

// --- adaptive scheduler (one per store) ---

// StartScheduler launches each store's GC loop: run GC, then sleep until the
// soonest record could age out (capped at Interval), waking early when a
// usage/distribute event arrives. Stores with Interval<=0 run manual GC only.
func (m *Manager) StartScheduler(ctx context.Context) {
	for _, u := range m.units {
		go u.runScheduler(ctx)
	}
}

func (u *unit) runScheduler(ctx context.Context) {
	if u.sched.Interval <= 0 {
		return // scheduling disabled; manual GC only
	}
	u.mu.Lock()
	u.started = u.m.now()
	u.mu.Unlock()
	for {
		u.mu.Lock()
		u.running = true
		u.mu.Unlock()
		dec := u.gcOnce(ctx)
		u.mu.Lock()
		u.running = false
		u.mu.Unlock()
		if u.m.onRun != nil {
			u.m.onRun(dec)
		}
		d := u.nextWake(dec)
		u.mu.Lock()
		u.wakeAt = u.m.now().Add(d)
		u.mu.Unlock()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-u.signal:
			timer.Stop()
			d := u.sched.MinInterval - u.m.now().Sub(u.lastRunAt())
			if d < 0 {
				d = 0
			}
			u.mu.Lock()
			u.wakeAt = u.m.now().Add(d)
			u.mu.Unlock()
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

func (u *unit) gcOnce(ctx context.Context) Decision {
	inv := u.reconcile(ctx)
	dec, err := u.plan(ctx, u.rules, u.untaggedAfter, inv)
	if err != nil {
		log.From(ctx).Warn("gc plan failed", slog.String("store", u.name), slog.String("error", err.Error()))
		return Decision{}
	}
	u.apply(ctx, dec)
	u.mu.Lock()
	u.lastRun = u.m.now()
	u.mu.Unlock()
	return dec
}

func (u *unit) lastRunAt() time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastRun
}

func (u *unit) poke() {
	select {
	case u.signal <- struct{}{}:
	default:
	}
}

func (u *unit) nextWake(dec Decision) time.Duration {
	cap := u.sched.Interval
	next := dec.NextAgeOut
	if next.IsZero() {
		return cap // nothing aging -> idle until Interval (or an event pokes)
	}
	d := next.Sub(u.m.now())
	if d < u.sched.MinInterval {
		d = u.sched.MinInterval
	}
	if d > cap {
		d = cap
	}
	return d
}
