package cpx

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fallbackCopier builds the two-hop shape a fallback is about: an origin
// registry that holds the image, an empty cache registry the job is pointed at,
// and one engine. Two SEPARATE registries are required — a single ggcr
// in-memory registry keys blobs by digest across every repository, so one
// server could never model "the cache does not have it".
func fallbackCopier(t *testing.T, eng *fakePullEngine) (w *Copier, js Store, origin, cache string) {
	t.Helper()
	origin, cache = startRegistry(t), startRegistry(t)
	w, js = newCopier(t, []config.StoreConfig{
		{Name: "origin", Kind: "oci", Host: origin, Insecure: true},
		{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	return w, js, origin, cache
}

func boolp(v bool) *bool { return &v }

// failHost fails a pull whose reference names host, so one source can be broken
// while the other serves the same image.
func failHost(host string, err error) func(string) error {
	return func(ref string) error {
		if strings.HasPrefix(ref, host+"/") {
			return err
		}
		return nil
	}
}

// A cache that cannot serve the image is a miss, not an outage: the pull is
// re-attempted against the origin named in the job's own ref, the job completes,
// and the failed attempt stays on the record as its own transfer.
func TestEnginePullFallsBackToOrigin(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	eng.failFor = failHost(cache, errors.New("MANIFEST_UNKNOWN: manifest unknown"))
	pushImage(t, origin+"/team/app:1", 2)

	var stamped []string
	w.SetPullHook(func(_, ref string) { stamped = append(stamped, ref) })

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}

	calls := eng.pulls()
	if len(calls) != 2 {
		t.Fatalf("pull attempts = %d, want 2 (cache then origin)", len(calls))
	}
	if want := cache + "/team/app:1"; calls[0].ref != want {
		t.Errorf("attempt 1 pulled %q, want the cache ref %q", calls[0].ref, want)
	}
	if want := origin + "/team/app:1"; calls[1].ref != want {
		t.Errorf("attempt 2 pulled %q, want the origin ref %q", calls[1].ref, want)
	}

	if len(done.Transfers) != 2 {
		t.Fatalf("transfers = %d, want one per attempt", len(done.Transfers))
	}
	if t0 := done.Transfers[0]; t0.State != "failed" || t0.Source != "cache" || t0.Err == "" {
		t.Errorf("transfer[0] = %+v, want a failed cache attempt carrying its error", t0)
	}
	if t1 := done.Transfers[1]; t1.State != "done" || t1.Source != "origin" {
		t.Errorf("transfer[1] = %+v, want a done origin attempt", t1)
	}
	if done.Transfers[1].BytesTotal == 0 {
		t.Error("the winning attempt should be sized from the origin manifest")
	}
	// Retention must track what the daemon actually holds — the origin ref.
	if len(stamped) != 1 || stamped[0] != origin+"/team/app:1" {
		t.Errorf("pull hook stamped %v, want only the origin ref", stamped)
	}
}

// Without the flag the job fails exactly as it does today: one transfer, one
// attempt, and the source's error is the job's error.
func TestEnginePullWithoutFallbackFails(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	eng.failFor = failHost(cache, errors.New("MANIFEST_UNKNOWN: manifest unknown"))
	pushImage(t, origin+"/team/app:1", 2)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: origin + "/team/app:1", Source: "cache", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobFailed {
		t.Fatalf("state = %q, want failed", done.State)
	}
	if n := len(eng.pulls()); n != 1 {
		t.Errorf("pull attempts = %d, want 1", n)
	}
	if len(done.Transfers) != 1 {
		t.Errorf("transfers = %d, want 1", len(done.Transfers))
	}
	if !strings.Contains(done.Err, "manifest unknown") {
		t.Errorf("job error = %q, want the source's error", done.Err)
	}
}

// An engine-side failure — a capability the daemon lacks, or a step after the
// content already arrived — is not a source fault: pulling the same image from
// somewhere else would fail identically, so no attempt is spent on it.
func TestEnginePullDoesNotFallBackOnEngineFailure(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	eng.failFor = failHost(cache, fmt.Errorf("%w: cannot register digest names", down.ErrEngine))
	pushImage(t, origin+"/team/app:1", 2)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobFailed {
		t.Fatalf("state = %q, want failed", done.State)
	}
	if n := len(eng.pulls()); n != 1 {
		t.Errorf("pull attempts = %d, want 1 — an engine-side failure is not retried elsewhere", n)
	}
}

// A source reporting a cancellation is not a reason to try another one — but
// nobody withdrew this job, so it is recorded as a failure rather than as a
// cancellation the caller never asked for.
func TestEnginePullDoesNotFallBackOnCancel(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	eng.failFor = failHost(cache, context.Canceled)
	pushImage(t, origin+"/team/app:1", 2)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobFailed {
		t.Fatalf("state = %q, want failed: nothing withdrew this job", done.State)
	}
	if n := len(eng.pulls()); n != 1 {
		t.Errorf("pull attempts = %d, want 1 — a reported cancellation is not retried elsewhere", n)
	}
}

// A digest-pinned job falls back to the very digest it was pinned to, so the
// origin cannot serve different bytes than the cache would have.
func TestEnginePullFallbackKeepsThePinnedDigest(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	eng.failFor = failHost(cache, errors.New("connection refused"))
	src := pushImage(t, origin+"/team/app:1", 2)
	got, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	desc := got.Digest.String()

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, serr := w.Submit(Request{
		Ref: fmt.Sprintf("%s/team/app@%s", origin, desc), Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if serr != nil {
		t.Fatalf("submit: %v", serr)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	calls := eng.pulls()
	if len(calls) != 2 {
		t.Fatalf("pull attempts = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if c.digest != desc {
			t.Errorf("attempt %d anchored to %q, want the pinned digest %q", i+1, c.digest, desc)
		}
	}
}

// The fallback is an engine-pull property; asking for it on a registry target is
// a mistake worth reporting rather than a no-op.
func TestFallbackRejectedForRegistryTarget(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, origin, _ := fallbackCopier(t, eng)
	w.base = context.Background()

	_, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "origin", Target: "cache",
		FallbackToOrigin: boolp(true),
	})
	if err == nil || !strings.Contains(err.Error(), "engine target") {
		t.Fatalf("err = %v, want a rejection naming the engine-target requirement", err)
	}
}

// pull_host points the daemon at one host no matter which registry gantry
// resolved, so both attempts would pull from the same place. A fallback the
// caller explicitly asked for and cannot get is an error, not a silent no-op.
func TestFallbackRejectedWhenPullHostCollapsesSources(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	origin, cache := startRegistry(t), startRegistry(t)
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "origin", Kind: "oci", Host: origin, Insecure: true},
		{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker", PullHost: "mirror.local"}, eng)
	w.base = context.Background()

	_, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err == nil || !strings.Contains(err.Error(), "not addressable") {
		t.Fatalf("err = %v, want a rejection naming the collapsed pull ref", err)
	}
}

// A server-wide default must not fail jobs it cannot apply to: an origin nobody
// declared simply means this job has no fallback, while the same situation is an
// error when the caller asked for the fallback by name.
func TestFallbackDefaultDegradesWhenOriginIsUndeclared(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	cache := startRegistry(t)
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	w.wc.FallbackToOrigin = true // the server default, not this caller's request
	w.base = context.Background()

	// "team/app:1" resolves its origin to index.docker.io, which is not a
	// declared store and cannot be synthesized (allow_unknown_stores is off).
	res, err := w.Plan(context.Background(), Request{Ref: "team/app:1", Source: "cache", Target: "node"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if res.FallbackToOrigin || res.FallbackRef != "" {
		t.Errorf("plan = {fallback:%v ref:%q}, want no fallback reported", res.FallbackToOrigin, res.FallbackRef)
	}

	if _, err := w.Plan(context.Background(), Request{
		Ref: "team/app:1", Source: "cache", Target: "node", FallbackToOrigin: boolp(true),
	}); err == nil {
		t.Error("an explicitly requested fallback to an undeclared origin should be rejected")
	}
}

// The fallback decision is part of what makes two moves interchangeable: a
// submit that refused the origin must not be handed a job allowed to use it.
func TestFallbackSplitsTheDedupKey(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, origin, _ := fallbackCopier(t, eng)
	w.base = context.Background() // enqueue without workers; jobs stay active

	req := Request{Ref: origin + "/team/app:1", Source: "cache", Target: "node", FallbackToOrigin: boolp(true)}
	if _, created, err := w.Submit(req); err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}
	same := req
	if _, created, err := w.Submit(same); err != nil || created {
		t.Fatalf("identical submit should coalesce: created=%v err=%v", created, err)
	}
	off := req
	off.FallbackToOrigin = boolp(false)
	if _, created, err := w.Submit(off); err != nil || !created {
		t.Fatalf("a submit that refuses the origin must not coalesce: created=%v err=%v", created, err)
	}
}

// Plan is the preflight for the fallback too: it reports the effective decision
// and the ref the engine would be told to pull.
func TestPlanReportsTheFallbackBinding(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, origin, _ := fallbackCopier(t, eng)

	res, err := w.Plan(context.Background(), Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !res.FallbackToOrigin {
		t.Error("plan should report the effective fallback decision")
	}
	if want := origin + "/team/app:1"; res.FallbackRef != want {
		t.Errorf("fallback ref = %q, want %q", res.FallbackRef, want)
	}

	// A job already pulling from the origin has nowhere to fall back to, which
	// is a normal shape rather than a misconfiguration.
	res, err = w.Plan(context.Background(), Request{
		Ref: origin + "/team/app:1", Source: "origin", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if res.FallbackRef != "" {
		t.Errorf("fallback ref = %q, want empty when the source already is the origin", res.FallbackRef)
	}
}

// A source that is merely not filled YET is not a miss. When another job is
// filling it right now, the pull waits for that job and re-attempts the same
// source — the origin is never contacted.
func TestEnginePullWaitsForAnInFlightFill(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	w.wc.SourceWait = config.Duration(10 * time.Second)
	pushImage(t, origin+"/team/app:1", 2)

	// A job that is putting exactly this image into the cache, still running.
	fill := NewJob("job_fill", origin+"/team/app:1", nil, time.Now())
	fill.Fills = []string{mustRefName(t, cache+"/team/app:1")}
	if err := js.Add(fill); err != nil {
		t.Fatal(err)
	}

	// The cache misses once, then serves it — as it would the moment the fill
	// above commits.
	var attempts atomic.Int32
	missed := make(chan struct{})
	eng.failFor = func(ref string) error {
		if !strings.HasPrefix(ref, cache+"/") {
			return nil
		}
		if attempts.Add(1) == 1 {
			close(missed)
			return errors.New("MANIFEST_UNKNOWN: manifest unknown")
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-missed        // the pull has missed and is now waiting on the fill
	fill.markDone() // the fill finishes

	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	calls := eng.pulls()
	if len(calls) != 2 {
		t.Fatalf("pull attempts = %d, want 2 (miss, then the same source again)", len(calls))
	}
	for i, c := range calls {
		if !strings.HasPrefix(c.ref, cache+"/") {
			t.Errorf("attempt %d pulled %q; the origin must not be contacted when the fill delivered", i+1, c.ref)
		}
	}
	if len(done.Transfers) != 2 || done.Transfers[1].Source != "cache" {
		t.Errorf("transfers = %+v, want a second cache attempt", done.Transfers)
	}
}

// Waiting is bounded: a fill that never finishes must not hold the pull open,
// and the fallback still gets its turn.
func TestEnginePullFallsBackWhenTheFillNeverFinishes(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	w.wc.SourceWait = config.Duration(50 * time.Millisecond)
	eng.failFor = failHost(cache, errors.New("MANIFEST_UNKNOWN: manifest unknown"))
	pushImage(t, origin+"/team/app:1", 2)

	fill := NewJob("job_fill", origin+"/team/app:1", nil, time.Now())
	fill.Fills = []string{mustRefName(t, cache+"/team/app:1")}
	if err := js.Add(fill); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	calls := eng.pulls()
	if len(calls) != 2 {
		t.Fatalf("pull attempts = %d, want 2 — the wait expires, it does not retry the source", len(calls))
	}
	if !strings.HasPrefix(calls[1].ref, origin+"/") {
		t.Errorf("attempt 2 pulled %q, want the origin after the wait expired", calls[1].ref)
	}
}

// Every worker parked on a fill is a worker not running one. When no wait slot
// is free the pull does not wait at all, so the pool always keeps draining.
func TestEnginePullSkipsTheWaitWhenNoSlotIsFree(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	w.wc.SourceWait = config.Duration(30 * time.Second) // would hang the test if taken
	eng.failFor = failHost(cache, errors.New("MANIFEST_UNKNOWN: manifest unknown"))
	pushImage(t, origin+"/team/app:1", 2)

	fill := NewJob("job_fill", origin+"/team/app:1", nil, time.Now())
	fill.Fills = []string{mustRefName(t, cache+"/team/app:1")}
	if err := js.Add(fill); err != nil {
		t.Fatal(err)
	}
	// Occupy every wait slot.
	for i := 0; i < cap(w.waitSlots); i++ {
		w.waitSlots <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	start := time.Now()
	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("job took %s — it waited for the fill despite having no slot", took)
	}
	if n := len(eng.pulls()); n != 2 {
		t.Errorf("pull attempts = %d, want 2 (miss then origin, no retry)", n)
	}
}

// Waiting is off unless configured: with source_wait unset an in-flight fill is
// invisible and the pull goes straight to the fallback.
func TestEnginePullDoesNotWaitWhenSourceWaitIsUnset(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	eng.failFor = failHost(cache, errors.New("MANIFEST_UNKNOWN: manifest unknown"))
	pushImage(t, origin+"/team/app:1", 2)

	fill := NewJob("job_fill", origin+"/team/app:1", nil, time.Now())
	fill.Fills = []string{mustRefName(t, cache+"/team/app:1")}
	if err := js.Add(fill); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if n := len(eng.pulls()); n != 2 {
		t.Errorf("pull attempts = %d, want 2 (miss then origin)", n)
	}
}

// A registry-target job advertises what it fills, and that string is exactly
// what an engine pull reading from that store looks for — the join a wait
// depends on. Verified through plan() rather than by construction.
func TestFillRefMatchesWhatAPullLooksFor(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, origin, _ := fallbackCopier(t, eng)
	w.base = context.Background()
	pushImage(t, origin+"/team/app:1", 1)

	fillPlan, err := w.plan(context.Background(), Request{
		Ref: origin + "/team/app:1", Source: "origin", Target: "cache",
	})
	if err != nil {
		t.Fatalf("plan fill: %v", err)
	}
	pullPlan, err := w.plan(context.Background(), Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
	})
	if err != nil {
		t.Fatalf("plan pull: %v", err)
	}
	fills := fillPlan.fills()
	if len(fills) != 1 {
		t.Fatalf("a registry-target job must advertise the ref it fills, got %v", fills)
	}
	want := pullPlan.last().attempts[0].waitFill
	if fills[0] != want {
		t.Errorf("fills = %q but a pull looks for %q; a wait would never match", fills[0], want)
	}
	if got := pullPlan.fills(); len(got) != 0 {
		t.Errorf("an engine-target job fills nothing another job reads, got %v", got)
	}
}

// mustRefName canonicalizes a reference the same way plan() does, so a test can
// state the fill ref without hard-coding go-containerregistry's normalization.
func mustRefName(t *testing.T, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return r.Name()
}

// The reported decision is what the job can actually do, not what was asked
// for: a job whose source already IS the origin has nowhere to fall back to, so
// it reports no fallback — and coalesces with an identical job that never asked
// for one, since the two behave the same.
func TestFallbackWithNowhereToFallBackIsNotReported(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, origin, _ := fallbackCopier(t, eng)
	w.base = context.Background()

	res, err := w.Plan(context.Background(), Request{
		Ref: origin + "/team/app:1", Source: "origin", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if res.FallbackToOrigin {
		t.Error("a job with no second source must not report a fallback it cannot perform")
	}

	req := Request{Ref: origin + "/team/app:1", Source: "origin", Target: "node", FallbackToOrigin: boolp(true)}
	if _, created, err := w.Submit(req); err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}
	off := req
	off.FallbackToOrigin = boolp(false)
	if _, created, err := w.Submit(off); err != nil || created {
		t.Fatalf("two jobs that behave identically should coalesce: created=%v err=%v", created, err)
	}
}

// A job canceled mid-pull must not launder the cancellation into a fallback:
// the caller asked for the work to stop, and the source never failed. This
// covers the branch a canceled CONTEXT takes, as distinct from an engine that
// happens to return context.Canceled.
func TestEnginePullDoesNotFallBackWhenTheJobIsCanceled(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	pushImage(t, origin+"/team/app:1", 2)

	started, release := make(chan struct{}), make(chan struct{})
	eng.failFor = func(ref string) error {
		if !strings.HasPrefix(ref, cache+"/") {
			return nil
		}
		close(started)
		<-release
		return errors.New("pull aborted") // not context.Canceled
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	exec, ok := js.Job(snap.ID)
	if !ok {
		t.Fatal("job not found")
	}
	<-started
	if _, ok, _ := js.Cancel(snap.ID); !ok {
		t.Fatal("cancel: job not found")
	}
	close(release)

	// Wait on the EXECUTION, not the handle: cancelling a handle makes its
	// snapshot read terminal immediately (the shared move may run on for other
	// callers), so waiting on the snapshot would return before the worker had a
	// chance to make a second attempt — and the assertion below would hold no
	// matter what the code did.
	select {
	case <-exec.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("execution did not finish")
	}
	if n := len(eng.pulls()); n != 1 {
		t.Errorf("pull attempts = %d, want 1 — a canceled job does not try another source", n)
	}
	if snap, ok := js.Snapshot(snap.ID); !ok || snap.State != JobCanceled {
		t.Errorf("handle state = %q (ok=%v), want canceled", snap.State, ok)
	}
}

// recordingRecorder captures the audit calls the copier makes.
type recordingRecorder struct {
	mu       sync.Mutex
	fellBack []string // "id|ref|from|to" per call
	causes   []string
	admitted int
	finished int
}

func (r *recordingRecorder) JobAdmitted(string, string, string, string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.admitted++
}

func (r *recordingRecorder) JobFinished(string, string, string, string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished++
}

func (r *recordingRecorder) JobFellBack(id, ref, from, to, cause string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fellBack = append(r.fellBack, strings.Join([]string{id, ref, from, to}, "|"))
	r.causes = append(r.causes, cause)
}

func (r *recordingRecorder) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.fellBack...), append([]string(nil), r.causes...)
}

// The durable record of a fallback is written when it happens — the in-memory
// job is emptied on restart, so this event is the only lasting trace that a
// cache stopped being used.
func TestFallbackIsAudited(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, origin, cache := fallbackCopier(t, eng)
	rec := &recordingRecorder{}
	w.SetRecorder(rec)
	eng.failFor = failHost(cache, errors.New("MANIFEST_UNKNOWN: manifest unknown"))
	pushImage(t, origin+"/team/app:1", 2)

	mctx, reader := otxContext(t)
	ctx, cancel := context.WithCancel(mctx)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if done := waitTerminal(t, js, snap.ID); done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	calls, causes := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("JobFellBack calls = %v, want exactly one", calls)
	}
	if want := strings.Join([]string{snap.ID, origin + "/team/app:1", "cache", "origin"}, "|"); calls[0] != want {
		t.Errorf("audited %q, want %q", calls[0], want)
	}
	if !strings.Contains(causes[0], "manifest unknown") {
		t.Errorf("audited cause = %q, want the source's error", causes[0])
	}

	// The counter is the signal an operator watches for a cache falling out of
	// use, so it must carry which store gave up and which took over.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	if got, ok := counterValue(t, rm, "gantry.job.fallback", "from", "cache"); !ok || got != 1 {
		t.Errorf("gantry.job.fallback{from=cache} = %d (found=%v), want 1", got, ok)
	}
	if _, ok := counterValue(t, rm, "gantry.job.fallback", "to", "origin"); !ok {
		t.Error("gantry.job.fallback should be labelled with the store that took over")
	}

	// A job served by its own source is not a fallback and must not be audited
	// as one.
	eng.failFor = nil
	snap2, _, err := w.Submit(Request{
		Ref: origin + "/team/app:1", Source: "origin", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitTerminal(t, js, snap2.ID)
	if calls, _ := rec.snapshot(); len(calls) != 1 {
		t.Errorf("JobFellBack calls = %v, want no new one for a job its source served", calls)
	}
}

// The anchor bytes backing a digest `as` name come from the store the winning
// attempt pulled from. Fetching them from the source that just failed would
// fail the whole job — and, on a source that answers with different content,
// would name the node's image after bytes it does not hold.
func TestFallbackFetchesTheAnchorFromTheServingSource(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	origin, cache := startRegistry(t), startRegistry(t)
	// Which host is contacted follows the REF, not the store config — the config
	// supplies credentials and transport — so two interchangeable stores would
	// hide a mixup entirely. Give the cache a transport that cannot be BUILT: an
	// origin attempt carrying the cache's config then fails outright instead of
	// quietly reaching the origin with the wrong credentials.
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "origin", Kind: "oci", Host: origin, Insecure: true},
		{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy",
			CACert: filepath.Join(t.TempDir(), "no-such-ca.pem")},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	eng.failFor = failHost(cache, errors.New("connection refused"))
	src := pushImage(t, origin+"/team/app:1", 2)
	got, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	dg := got.Digest.String()

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	// A digest `as` name requires a digest-pinned job; the cache holds nothing,
	// so only the origin can supply the anchor manifest.
	snap, _, serr := w.Submit(Request{
		Ref: fmt.Sprintf("%s/team/app@%s", origin, dg), Source: "cache", Target: "node",
		As:               []string{fmt.Sprintf("%s/team/app@%s", origin, dg)},
		FallbackToOrigin: boolp(true),
	})
	if serr != nil {
		t.Fatalf("submit: %v", serr)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	// The job completing at all is the proof: the anchor is fetched BEFORE the
	// pull, so the cache attempt died at its own anchor fetch (the cache holds
	// nothing) without ever reaching the daemon. Had the anchor always come from
	// the first binding, the origin attempt would have died the same way and the
	// job would have failed.
	calls := eng.pulls()
	if len(calls) != 1 {
		t.Fatalf("pull attempts = %d, want 1 — the cache attempt fails before the daemon is called", len(calls))
	}
	if !strings.HasPrefix(calls[0].ref, origin+"/") {
		t.Errorf("the daemon was sent to %q, want the origin", calls[0].ref)
	}
	if calls[0].anchor == nil {
		t.Fatal("the origin attempt carried no anchor manifest")
	}
	if calls[0].anchor.Digest != dg {
		t.Errorf("anchor digest = %q, want %q", calls[0].anchor.Digest, dg)
	}
}

// The ref a pull looks for is captured BEFORE verification pins the source to a
// digest, because the fill it may be waiting on derives its own ref from the
// tag. Pinning first would make the two never match.
func TestFillWantIsCapturedBeforePinning(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, origin, cache := fallbackCopier(t, eng)
	w.base = context.Background()
	src := pushImage(t, origin+"/team/app:1", 1)
	got, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	w.SetVerifier(&fakeVerifier{dg: got.Digest})

	p, err := w.plan(context.Background(), Request{
		Ref: origin + "/team/app:1", Source: "cache", Target: "node",
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.digest() == "" {
		t.Fatal("the verifier should have pinned the plan; the test proves nothing otherwise")
	}
	if want := mustRefName(t, cache+"/team/app:1"); p.last().attempts[0].waitFill != want {
		t.Errorf("waitFill = %q, want the pre-pinning tag ref %q", p.last().attempts[0].waitFill, want)
	}
}

// Each source is addressed under its OWN store's options: http-vs-https is baked
// into the parsed reference, so an insecure cache must not lend its scheme to a
// TLS origin.
func TestFallbackBindingUsesTheOriginStoresOptions(t *testing.T) {
	// Named hosts, not loopback: go-containerregistry treats localhost/127.0.0.1
	// as insecure on its own, which would mask the very thing under test. This
	// only plans — nothing is dialled.
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "origin", Kind: "oci", Host: "origin.example"}, // TLS
		{Name: "cache", Kind: "oci", Host: "cache.example", Insecure: true, Mode: "copy"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	w.srcOpts = nil // do not force every ref insecure, as the shared helper does
	w.base = context.Background()

	p, err := w.plan(context.Background(), Request{
		Ref: "origin.example/team/app:1", Source: "cache", Target: "node",
		FallbackToOrigin: boolp(true),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	ats := p.last().attempts
	if len(ats) != 2 {
		t.Fatalf("attempts = %d, want the origin bound as an alternative", len(ats))
	}
	if got := ats[0].ref.Context().Scheme(); got != "http" {
		t.Errorf("cache attempt scheme = %q, want http (the store is insecure)", got)
	}
	if got := ats[1].ref.Context().Scheme(); got != "https" {
		t.Errorf("origin attempt scheme = %q, want https — it must not inherit the cache's", got)
	}
}

// counterValue reads one Int64 counter data point, selected by attribute.
func counterValue(t *testing.T, rm metricdata.ResourceMetrics, name, attr_key, attr_val string) (int64, bool) {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data = %T, want int64 sum", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(attr_key)); ok && v.AsString() == attr_val {
					return dp.Value, true
				}
			}
		}
	}
	return 0, false
}
