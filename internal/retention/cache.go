package retention

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/otx/log"
	bolt "go.etcd.io/bbolt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// A registry store's retention has no policy of its own. It FOLLOWS the
// engines: what gantry delivered through the registry stays there while any
// engine store's retention still holds it, and goes once every one of them has
// dropped it.
//
// "Holds" is read off the engines' retention indexes, not off the daemons. A
// record leaves an index only when gantry's own GC (or an operator's Remove)
// drops it, so an image a node lost some other way — pruned, re-imaged — is
// still held, and the registry keeps the copy that node is about to need. What
// the registry keeps is therefore exactly the engines' keep_n, pins and in-use
// protection, with nothing to configure on the registry itself.
//
// The registry cannot be asked what it holds (a digest pin has no tag, so no
// tag list shows it), so gantry records what it delivered through it: every
// engine pull that read from it, and every fill of it gantry added for a route.
// Nothing else is ever deleted — whatever reached the registry without gantry
// is the registry's own garbage collection's business.

// Delivery is one manifest gantry delivered through a registry store.
type Delivery struct {
	Repo   string   `json:"repo"`           // repository path in the registry, no host
	Digest string   `json:"digest"`         // the manifest (or index) digest
	Tags   []string `json:"tags,omitempty"` // tags it was delivered under that still point at it
	// DateDelivered is the last time gantry delivered it: a record is kept for
	// the grace after it, so an engine has recorded what was just filled before
	// the fill can look dropped.
	DateDelivered time.Time `json:"date_delivered"`
}

// Ref is the delivery's key and the reference a delete is issued for.
func (d Delivery) Ref() string { return d.Repo + "@" + d.Digest }

// Deliver records that gantry delivered repo@digest through a registry store at
// t, under tag when the delivery named one. A tag belongs to one manifest at a
// time: recording it here takes it off any other delivery of the same
// repository, so an older manifest the tag has moved away from is no longer
// held by an engine still holding the tag.
func (ix *Index) Deliver(registry, repo, digest, tag string, t time.Time) error {
	key := repo + "@" + digest
	return ix.db.Update(func(tx *bolt.Tx) error {
		b, err := sub(tx, bktDlv, registry)
		if err != nil {
			return err
		}
		var d Delivery
		if v := b.Get([]byte(key)); v != nil {
			_ = json.Unmarshal(v, &d)
		}
		d.Repo, d.Digest = repo, digest
		if t.After(d.DateDelivered) {
			d.DateDelivered = t
		}
		if tag != "" && !slices.Contains(d.Tags, tag) {
			d.Tags = append(d.Tags, tag)
		}
		if tag != "" {
			if err := untagOthers(b, repo, tag, key); err != nil {
				return err
			}
		}
		enc, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), enc)
	})
}

func untagOthers(b *bolt.Bucket, repo, tag, keep string) error {
	type upd struct {
		k []byte
		v []byte
	}
	var ups []upd
	err := b.ForEach(func(k, v []byte) error {
		if string(k) == keep {
			return nil
		}
		var d Delivery
		if json.Unmarshal(v, &d) != nil || d.Repo != repo || !slices.Contains(d.Tags, tag) {
			return nil
		}
		d.Tags = slices.DeleteFunc(d.Tags, func(s string) bool { return s == tag })
		enc, err := json.Marshal(d)
		if err != nil {
			return err
		}
		ups = append(ups, upd{append([]byte(nil), k...), enc})
		return nil
	})
	if err != nil {
		return err
	}
	for _, u := range ups {
		if err := b.Put(u.k, u.v); err != nil {
			return err
		}
	}
	return nil
}

// Deliveries returns every delivery recorded for a registry store.
func (ix *Index) Deliveries(registry string) ([]Delivery, error) {
	var out []Delivery
	err := ix.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktDlv).Bucket([]byte(registry))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var d Delivery
			if json.Unmarshal(v, &d) == nil {
				out = append(out, d)
			}
			return nil
		})
	})
	return out, err
}

func (ix *Index) delivery(registry, ref string) (Delivery, bool) {
	var d Delivery
	ok := false
	_ = ix.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktDlv).Bucket([]byte(registry))
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(ref)); v != nil {
			ok = json.Unmarshal(v, &d) == nil
		}
		return nil
	})
	return d, ok
}

// DeleteDelivery removes one delivery record, reporting whether it existed.
func (ix *Index) DeleteDelivery(registry, ref string) (bool, error) {
	existed := false
	err := ix.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktDlv).Bucket([]byte(registry))
		if b == nil {
			return nil
		}
		existed = b.Get([]byte(ref)) != nil
		return b.Delete([]byte(ref))
	})
	return existed, err
}

func (ix *Index) countDeliveries(registry string) int {
	n := 0
	_ = ix.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bktDlv).Bucket([]byte(registry)); b != nil {
			n = b.Stats().KeyN
		}
		return nil
	})
	return n
}

// Registry describes one registry store's retention, passed to WithRegistries.
type Registry struct {
	Name     string
	Index    *Index
	Schedule Schedule // Heartbeat is an engine's and is ignored
	// Remove deletes one manifest, "repo@digest", from the registry. A manifest
	// that is already gone is not an error: the record is what is left to drop.
	Remove func(ctx context.Context, ref string) error
	// Engines is every engine store declared, with retention or without. One
	// without keeps no record of what it dropped, so it can never say it no
	// longer holds something, and nothing is deleted while it is declared.
	Engines []string
	// Hosts are the names a node pulls this registry by: its host, its
	// downstream_host, and any engine's pull_host. An engine record naming a
	// TAG holds a delivery only under one of these — the same path and tag on
	// another registry is another image. A digest holds wherever it is named,
	// since it is the same content wherever it came from.
	Hosts []string
}

// WithRegistries adds registry stores whose retention follows the engines.
func WithRegistries(rs ...Registry) Option {
	return func(m *Manager) {
		for _, r := range rs {
			m.regs[r.Name] = &registryUnit{
				m: m, name: r.Name, ix: r.Index, sched: r.Schedule,
				remove: r.Remove, engines: r.Engines, hosts: canonicalHosts(r.Hosts), signal: make(chan struct{}, 1),
			}
		}
	}
}

// canonicalHosts spells each host the way a parsed reference reports its
// registry ("docker.io" is "index.docker.io"), dropping the empty ones.
func canonicalHosts(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if r, err := name.NewRegistry(h, name.WeakValidation); err == nil {
			h = r.RegistryStr()
		}
		out = append(out, h)
	}
	return out
}

// registryUnit is one registry store's retention state.
type registryUnit struct {
	m       *Manager
	name    string
	ix      *Index
	sched   Schedule
	remove  func(ctx context.Context, ref string) error
	engines []string
	hosts   []string

	signal chan struct{}

	mu      sync.Mutex
	started time.Time
	lastRun time.Time
	wakeAt  time.Time
	running bool
}

// Delivered records a delivery through a registry store; a store without
// retention records nothing.
func (m *Manager) Delivered(registry, repo, digest, tag string, t time.Time) {
	r, ok := m.regs[registry]
	if !ok || digest == "" {
		return
	}
	if err := r.ix.Deliver(registry, repo, digest, tag, t); err != nil {
		log.From(context.Background()).Warn("recording a delivery failed",
			slog.String("store", registry), slog.String("ref", repo+"@"+digest), slog.String("error", err.Error()))
	}
}

// TracksDeliveries reports whether a store's deliveries are recorded: whether
// it is a registry store with retention.
func (m *Manager) TracksDeliveries(registry string) bool {
	_, ok := m.regs[registry]
	return ok
}

// Deliveries returns what gantry has delivered through a registry store.
func (m *Manager) Deliveries(registry string) ([]Delivery, bool, error) {
	r, ok := m.regs[registry]
	if !ok {
		return nil, false, nil
	}
	ds, err := r.ix.Deliveries(registry)
	return ds, true, err
}

// pokeRegistries wakes every registry's scheduler: an engine just dropped
// something, which may be the last hold on a delivery.
func (m *Manager) pokeRegistries() {
	for _, r := range m.regs {
		select {
		case r.signal <- struct{}{}:
		default:
		}
	}
}

// held is what the engines' retention indexes still hold: digests, and the
// tags of records that name a tag under one of the registry's hosts, keyed by
// repository path so a record under a node's pull host matches a delivery
// under the registry's own.
type held struct {
	digests map[string]bool
	tags    map[string]bool // "<repo path>:<tag>"
}

func (h held) has(d Delivery) bool {
	if h.digests[d.Digest] {
		return true
	}
	for _, t := range d.Tags {
		if h.tags[d.Repo+":"+t] {
			return true
		}
	}
	return false
}

func (m *Manager) heldByEngines(hosts []string) (held, error) {
	h := held{digests: map[string]bool{}, tags: map[string]bool{}}
	for engine, u := range m.units {
		recs, err := u.ix.List(engine)
		if err != nil {
			return held{}, err
		}
		for _, r := range recs {
			if r.Digest != "" {
				h.digests[r.Digest] = true
			}
			ref, err := name.ParseReference(r.Ref, name.WeakValidation)
			if err != nil {
				continue
			}
			if t, ok := ref.(name.Tag); ok && slices.Contains(hosts, ref.Context().RegistryStr()) {
				h.tags[ref.Context().RepositoryStr()+":"+t.TagStr()] = true
			}
		}
	}
	return h, nil
}

// unmanagedEngine is the first declared engine store with no retention, or "".
func (r *registryUnit) unmanagedEngine() string {
	for _, e := range r.engines {
		if _, ok := r.m.units[e]; !ok {
			return e
		}
	}
	return ""
}

func (r *registryUnit) plan() (Decision, error) {
	dlv, err := r.ix.Deliveries(r.name)
	if err != nil {
		return Decision{}, err
	}
	var dec Decision
	if e := r.unmanagedEngine(); e != "" {
		for _, d := range dlv {
			dec.Keep = append(dec.Keep, Kept{Ref: d.Ref(), Reason: "engine_unmanaged"})
		}
		return dec, nil
	}
	h, err := r.m.heldByEngines(r.hosts)
	if err != nil {
		return Decision{}, err
	}
	now := r.m.now()
	graceUntil := r.graceUntil()
	ageOut := func(t time.Time) {
		if dec.NextAgeOut.IsZero() || t.Before(dec.NextAgeOut) {
			dec.NextAgeOut = t
		}
	}
	for _, d := range dlv {
		until := d.DateDelivered.Add(r.sched.Grace)
		switch {
		case h.has(d):
			dec.Keep = append(dec.Keep, Kept{Ref: d.Ref(), Reason: "held_by_engine"})
		case now.Before(until):
			dec.Keep = append(dec.Keep, Kept{Ref: d.Ref(), Reason: "recently_delivered"})
			ageOut(until)
		case now.Before(graceUntil):
			dec.Keep = append(dec.Keep, Kept{Ref: d.Ref(), Reason: "grace"})
			ageOut(graceUntil)
		default:
			dec.Delete = append(dec.Delete, Candidate{
				Ref: d.Ref(), Digest: d.Digest, LastUsed: d.DateDelivered, Reason: "dropped_by_engines",
			})
		}
	}
	return dec, nil
}

// apply deletes a decision's candidates from the registry. Each is checked
// again first, since the decision may be older than the state: a delivery made
// since the plan, or an engine that took the image back, keeps it.
func (r *registryUnit) apply(ctx context.Context, dec Decision) ApplyResult {
	res := ApplyResult{Evaluated: len(dec.Delete) + len(dec.Keep)}
	if len(dec.Delete) == 0 {
		return res
	}
	if e := r.unmanagedEngine(); e != "" {
		return res
	}
	h, err := r.m.heldByEngines(r.hosts)
	if err != nil {
		res.Errors = append(res.Errors, "engine indexes: "+err.Error())
		return res
	}
	for _, c := range dec.Delete {
		d, ok := r.ix.delivery(r.name, c.Ref)
		if !ok || h.has(d) || d.DateDelivered.After(c.LastUsed) {
			continue
		}
		if err := r.remove(ctx, c.Ref); err != nil {
			res.Errors = append(res.Errors, c.Ref+": "+err.Error())
			continue
		}
		res.Deleted = append(res.Deleted, c.Digest)
		_, _ = r.ix.DeleteDelivery(r.name, c.Ref)
		if r.m.rec != nil {
			r.m.rec.ImageRemoved(r.name, c.Ref, c.Digest, c.Reason)
		}
	}
	if len(res.Deleted) > 0 {
		log.From(ctx).Info("gc collected", slog.String("store", r.name),
			slog.Int("deleted", len(res.Deleted)), slog.Int("evaluated", res.Evaluated))
		if r.m.rec != nil {
			r.m.rec.GCApplied(r.name, len(res.Deleted), 0, 0, len(res.Errors))
		}
	}
	if len(res.Errors) > 0 {
		log.From(ctx).Warn("gc removal errors", slog.String("store", r.name), slog.Int("count", len(res.Errors)))
	}
	if r.m.cDeleted != nil {
		attr := metric.WithAttributes(attribute.String("store", r.name))
		r.m.cDeleted.Add(ctx, int64(len(res.Deleted)), attr)
		r.m.cErrors.Add(ctx, int64(len(res.Errors)), attr)
	}
	return res
}

func (r *registryUnit) status() StoreStatus {
	r.mu.Lock()
	ss := StoreStatus{
		Running:    r.running,
		Started:    r.started,
		LastRun:    r.lastRun,
		NextWake:   r.wakeAt,
		GraceUntil: r.graceUntilLocked(),
	}
	r.mu.Unlock()
	ss.Schedule = ScheduleStatus{
		Interval:    r.sched.Interval.String(),
		MinInterval: r.sched.MinInterval.String(),
		Grace:       r.sched.Grace.String(),
	}
	ss.Rules = []RuleStatus{}
	ss.Records = r.ix.countDeliveries(r.name)
	return ss
}

func (r *registryUnit) graceUntil() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.graceUntilLocked()
}

func (r *registryUnit) graceUntilLocked() time.Time {
	if r.started.IsZero() || r.sched.Grace <= 0 {
		return time.Time{}
	}
	return r.started.Add(r.sched.Grace)
}

func (r *registryUnit) gcOnce(ctx context.Context) Decision {
	dec, err := r.plan()
	if err != nil {
		log.From(ctx).Warn("gc plan failed", slog.String("store", r.name), slog.String("error", err.Error()))
		return Decision{}
	}
	if e := r.unmanagedEngine(); e != "" && len(dec.Keep) > 0 {
		log.From(ctx).Debug("registry gc holds everything: an engine store keeps no retention index",
			slog.String("store", r.name), slog.String("engine", e))
	}
	r.apply(ctx, dec)
	r.mu.Lock()
	r.lastRun = r.m.now()
	r.mu.Unlock()
	return dec
}

// runScheduler is the engine scheduler's loop without the usage watcher: run,
// then sleep until the soonest delivery leaves its grace (capped at Interval),
// waking early when an engine drops something.
func (r *registryUnit) runScheduler(ctx context.Context) {
	if r.sched.Interval <= 0 {
		return
	}
	r.mu.Lock()
	r.started = r.m.now()
	r.mu.Unlock()
	for {
		r.mu.Lock()
		r.running = true
		r.mu.Unlock()
		dec := r.gcOnce(ctx)
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
		if r.m.onRun != nil {
			r.m.onRun(dec)
		}
		d := r.nextWake(dec)
		r.mu.Lock()
		r.wakeAt = r.m.now().Add(d)
		r.mu.Unlock()
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-r.signal:
			timer.Stop()
			r.mu.Lock()
			wait := r.sched.MinInterval - r.m.now().Sub(r.lastRun)
			r.mu.Unlock()
			if wait > 0 {
				deb := time.NewTimer(wait)
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

func (r *registryUnit) nextWake(dec Decision) time.Duration {
	d := r.sched.Interval
	if !dec.NextAgeOut.IsZero() {
		if until := dec.NextAgeOut.Sub(r.m.now()); until < d {
			d = until
		}
	}
	if d < r.sched.MinInterval {
		d = r.sched.MinInterval
	}
	if d < time.Second {
		d = time.Second
	}
	return d
}
