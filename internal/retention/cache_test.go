package retention

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const (
	dgA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dgB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeRegistry records the deletes a registry unit issues.
type fakeRegistry struct {
	mu   sync.Mutex
	rmed []string
	err  error
}

func (f *fakeRegistry) remove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.rmed = append(f.rmed, ref)
	return nil
}

func (f *fakeRegistry) removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rmed...)
}

// cacheFixture is one engine store `node` with retention and one registry
// store `cache` following it, on a hand-advanced clock.
type cacheFixture struct {
	m       *Manager
	node    *Index // the engine's retention index
	cache   *Index // the registry's delivery record
	reg     *fakeRegistry
	clock   *fakeClock
	engines []string
}

func newCacheFixture(t *testing.T, engines ...string) *cacheFixture {
	t.Helper()
	if len(engines) == 0 {
		engines = []string{"node"}
	}
	f := &cacheFixture{node: openTemp(t), cache: openTemp(t), reg: &fakeRegistry{}, clock: newClock(), engines: engines}
	f.m = NewManager(
		[]Store{{Name: "node", Engine: &fakeEng{name: "node"}, Index: f.node, Rules: blanketRules(Policy{})}},
		WithNow(f.clock.now),
		WithRegistries(Registry{
			Name: "cache", Index: f.cache, Schedule: Schedule{Grace: time.Hour},
			Remove: f.reg.remove, Engines: engines, Hosts: []string{"cache.local:5000"},
		}),
	)
	return f
}

func (f *cacheFixture) plan(t *testing.T) Decision {
	t.Helper()
	dec, err := f.m.Plan(context.Background(), "cache", nil)
	if err != nil {
		t.Fatal(err)
	}
	return dec
}

func decisionReason(dec Decision, ref string) string {
	for _, c := range dec.Delete {
		if c.Ref == ref {
			return c.Reason
		}
	}
	for _, k := range dec.Keep {
		if k.Ref == ref {
			return k.Reason
		}
	}
	return ""
}

// While an engine's retention holds the digest, the registry keeps it — under
// whatever host the node pulled it by.
func TestRegistryKeepsWhatAnEngineHolds(t *testing.T) {
	f := newCacheFixture(t)
	f.m.Delivered("cache", "dist/app", dgA, "", f.clock.now())
	_ = f.node.Touch("node", "cache.local:5000/dist/app@"+dgA, f.clock.now())
	f.clock.advance(2 * time.Hour)

	if got := decisionReason(f.plan(t), "dist/app@"+dgA); got != "held_by_engine" {
		t.Errorf("reason = %q, want held_by_engine", got)
	}
}

// Once no engine's retention holds it and the grace has passed, it goes — and
// the apply deletes exactly that manifest and forgets the delivery.
func TestRegistryDropsWhatEveryEngineDropped(t *testing.T) {
	f := newCacheFixture(t)
	f.m.Delivered("cache", "dist/app", dgA, "", f.clock.now())
	f.clock.advance(2 * time.Hour)

	dec := f.plan(t)
	if got := decisionReason(dec, "dist/app@"+dgA); got != "dropped_by_engines" {
		t.Fatalf("reason = %q, want dropped_by_engines", got)
	}
	res, err := f.m.Apply(context.Background(), "cache", dec)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.reg.removed(); len(got) != 1 || got[0] != "dist/app@"+dgA {
		t.Errorf("registry deletes = %v, want the one manifest", got)
	}
	if len(res.Deleted) != 1 || res.Deleted[0] != dgA {
		t.Errorf("result = %+v", res)
	}
	if ds, _ := f.cache.Deliveries("cache"); len(ds) != 0 {
		t.Errorf("deliveries after apply = %+v, want none", ds)
	}
}

// What was just delivered is kept for the grace: the engine may not have
// recorded it yet, and the fill of a routed job lands before its pull does.
func TestRegistryKeepsARecentDeliveryAndSaysWhenItAgesOut(t *testing.T) {
	f := newCacheFixture(t)
	at := f.clock.now()
	f.m.Delivered("cache", "dist/app", dgA, "", at)
	f.clock.advance(10 * time.Minute)

	dec := f.plan(t)
	if got := decisionReason(dec, "dist/app@"+dgA); got != "recently_delivered" {
		t.Errorf("reason = %q, want recently_delivered", got)
	}
	if want := at.Add(time.Hour); !dec.NextAgeOut.Equal(want) {
		t.Errorf("next age-out = %v, want %v", dec.NextAgeOut, want)
	}
}

// A declared engine with no retention keeps no record of what it dropped, so
// it can never say it no longer holds something: nothing goes.
func TestRegistryKeepsEverythingWhileAnEngineIsUnmanaged(t *testing.T) {
	f := newCacheFixture(t, "node", "laptop")
	f.m.Delivered("cache", "dist/app", dgA, "", f.clock.now())
	f.clock.advance(2 * time.Hour)

	dec := f.plan(t)
	if got := decisionReason(dec, "dist/app@"+dgA); got != "engine_unmanaged" {
		t.Errorf("reason = %q, want engine_unmanaged", got)
	}
	// Even a decision that says otherwise is not carried out.
	dec.Delete = []Candidate{{Ref: "dist/app@" + dgA, Digest: dgA, LastUsed: f.clock.now()}}
	if _, err := f.m.Apply(context.Background(), "cache", dec); err != nil {
		t.Fatal(err)
	}
	if got := f.reg.removed(); len(got) != 0 {
		t.Errorf("registry deletes = %v, want none", got)
	}
}

// An engine record that names a tag holds the delivery made under that tag.
// When the tag moves to a newer delivery, the older one loses it and goes.
func TestRegistryFollowsATagToItsNewestDelivery(t *testing.T) {
	f := newCacheFixture(t)
	f.m.Delivered("cache", "dist/app", dgA, "1", f.clock.now())
	_ = f.node.Touch("node", "cache.local:5000/dist/app:1", f.clock.now())
	f.clock.advance(2 * time.Hour)
	if got := decisionReason(f.plan(t), "dist/app@"+dgA); got != "held_by_engine" {
		t.Fatalf("reason = %q, want held_by_engine by the tag", got)
	}

	f.m.Delivered("cache", "dist/app", dgB, "1", f.clock.now())
	f.clock.advance(2 * time.Hour)
	dec := f.plan(t)
	if got := decisionReason(dec, "dist/app@"+dgA); got != "dropped_by_engines" {
		t.Errorf("old manifest: reason = %q, want dropped_by_engines once the tag moved", got)
	}
	if got := decisionReason(dec, "dist/app@"+dgB); got != "held_by_engine" {
		t.Errorf("new manifest: reason = %q, want held_by_engine", got)
	}
}

// A tag holds only under the registry's own hosts: the same path and tag on
// another registry is another image, and must not keep this one forever.
func TestRegistryIgnoresTheSameTagOnAnotherRegistry(t *testing.T) {
	f := newCacheFixture(t)
	f.m.Delivered("cache", "dist/app", dgA, "1", f.clock.now())
	_ = f.node.Touch("node", "elsewhere.local/dist/app:1", f.clock.now())
	f.clock.advance(2 * time.Hour)

	if got := decisionReason(f.plan(t), "dist/app@"+dgA); got != "dropped_by_engines" {
		t.Errorf("reason = %q, want dropped_by_engines: another registry's tag holds nothing here", got)
	}
}

// A decision can be older than the state. A delivery made since, or an engine
// that has taken the image back, keeps it.
func TestRegistryApplyChecksAgain(t *testing.T) {
	f := newCacheFixture(t)
	f.m.Delivered("cache", "dist/app", dgA, "", f.clock.now())
	f.m.Delivered("cache", "dist/app", dgB, "", f.clock.now())
	f.clock.advance(2 * time.Hour)
	dec := f.plan(t)
	if len(dec.Delete) != 2 {
		t.Fatalf("plan = %+v, want both dropped", dec)
	}

	f.m.Delivered("cache", "dist/app", dgA, "", f.clock.now())
	_ = f.node.Touch("node", "cache.local:5000/dist/app@"+dgB, f.clock.now())
	if _, err := f.m.Apply(context.Background(), "cache", dec); err != nil {
		t.Fatal(err)
	}
	if got := f.reg.removed(); len(got) != 0 {
		t.Errorf("registry deletes = %v, want none: both came back after the plan", got)
	}
}

// A registry that refuses the delete keeps the record, so the next pass asks
// again rather than forgetting what is still there.
func TestRegistryKeepsTheRecordWhenTheDeleteFails(t *testing.T) {
	f := newCacheFixture(t)
	f.reg.err = errors.New("405 UNSUPPORTED")
	f.m.Delivered("cache", "dist/app", dgA, "", f.clock.now())
	f.clock.advance(2 * time.Hour)

	res, err := f.m.Apply(context.Background(), "cache", f.plan(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 || len(res.Deleted) != 0 {
		t.Errorf("result = %+v, want one error and nothing deleted", res)
	}
	if ds, _ := f.cache.Deliveries("cache"); len(ds) != 1 {
		t.Errorf("deliveries = %+v, want the one still recorded", ds)
	}
}

// A registry's retention has no policy to override.
func TestRegistryPlanRefusesAnOverride(t *testing.T) {
	f := newCacheFixture(t)
	if _, err := f.m.Plan(context.Background(), "cache", &Policy{MaxAge: time.Hour}); err == nil {
		t.Error("an override was accepted for a registry store")
	}
}

// A store without registry retention records nothing.
func TestDeliveredIgnoresAnUntrackedStore(t *testing.T) {
	f := newCacheFixture(t)
	f.m.Delivered("origin", "dist/app", dgA, "", f.clock.now())
	if f.m.TracksDeliveries("origin") {
		t.Error("an untracked store reports tracking")
	}
	if ds, _ := f.cache.Deliveries("origin"); len(ds) != 0 {
		t.Errorf("deliveries = %+v", ds)
	}
}

// An engine's GC that drops something wakes the registry, whose last hold may
// have been exactly that.
func TestEngineGCWakesTheRegistry(t *testing.T) {
	f := newCacheFixture(t)
	_ = f.node.Touch("node", "cache.local:5000/dist/app@"+dgA, f.clock.now())
	if _, err := f.m.DeleteRecord("node", "cache.local:5000/dist/app@"+dgA); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.m.regs["cache"].signal:
	default:
		t.Error("the registry was not woken")
	}
}

func TestRegistryStatus(t *testing.T) {
	f := newCacheFixture(t)
	f.m.Delivered("cache", "dist/app", dgA, "", f.clock.now())
	st, ok := f.m.Status().Stores["cache"]
	if !ok {
		t.Fatal("no status for the registry store")
	}
	if st.Records != 1 || st.Schedule.Grace != time.Hour.String() {
		t.Errorf("status = %+v", st)
	}
}
