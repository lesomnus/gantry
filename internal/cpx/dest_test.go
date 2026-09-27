package cpx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
)

// pullCall is one Pull the fake engine was asked for; a job with a fallback
// source makes more than one, and their order is the attempt order.
type pullCall struct {
	ref      string
	digest   string
	platform string
	as       []string
	anchor   *down.AnchorBlob
}

// fakePullEngine is an engine daemon double: it records what it was told to
// pull, reports canned layer progress, and can fail like a daemon that lacks
// the requested platform.
type fakePullEngine struct {
	name     string
	platform string // host platform reported by Platform()

	mu       sync.Mutex
	ref      string
	digest   string
	as       []string
	anchor   *down.AnchorBlob
	pulled   string     // platform the pull asked for
	calls    []pullCall // every attempt, in order
	recorded []string   // canned Pull return (nil = echo the requested names)
	pullErr  error
	// failFor fails only some references, so one source can be broken while
	// another serves the same image. Consulted after pullErr.
	failFor  func(ref string) error
	reported []down.LayerUpdate
}

// pulls returns the attempts made so far.
func (f *fakePullEngine) pulls() []pullCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pullCall(nil), f.calls...)
}

func (f *fakePullEngine) Name() string                { return f.name }
func (f *fakePullEngine) Kind() string                { return "docker" }
func (f *fakePullEngine) Ready(context.Context) error { return nil }
func (f *fakePullEngine) Close() error                { return nil }
func (f *fakePullEngine) Platform(context.Context) (string, error) {
	if f.platform == "" {
		return "", errors.New("daemon unreachable")
	}
	return f.platform, nil
}
func (f *fakePullEngine) Pull(_ context.Context, ref, digest, platform string, as []string, anchor *down.AnchorBlob, sink down.Sink) ([]string, error) {
	f.mu.Lock()
	f.ref, f.digest, f.pulled, f.as, f.anchor = ref, digest, platform, as, anchor
	f.calls = append(f.calls, pullCall{ref: ref, digest: digest, platform: platform, as: as, anchor: anchor})
	reports := f.reported
	err := f.pullErr
	failFor := f.failFor
	recorded := f.recorded
	f.mu.Unlock()
	// Outside the lock: a test may block here to hold the pull open while it
	// does something to the job.
	if err == nil && failFor != nil {
		err = failFor(ref)
	}
	for _, u := range reports {
		sink.Layer(u)
	}
	if err != nil {
		return nil, err
	}
	if recorded != nil {
		return recorded, nil // canned: whatever the test wants the engine to report
	}
	// Like a real engine: the requested names, or the pull-created record.
	if len(as) == 0 {
		return []string{ref}, nil
	}
	return as, nil
}
func (f *fakePullEngine) InUse(context.Context) (map[string]bool, error) { return nil, nil }
func (f *fakePullEngine) SeedUsage(context.Context, down.UsageSink) error {
	return nil
}
func (f *fakePullEngine) WatchUsage(ctx context.Context, _ down.UsageSink) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakePullEngine) Remove(context.Context, string) (down.RemoveResult, error) {
	return down.RemoveResult{}, nil
}

// engineCopier builds a copier over one upstream registry (live httptest) and
// one fake engine store named "node".
func engineCopier(t *testing.T, eng *fakePullEngine) (*Copier, Store, string) {
	t.Helper()
	up := startRegistry(t)
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: up, Insecure: true},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker"}, eng)
	return w, js, up
}

// A job whose `target` is an engine store makes the daemon pull the image: the
// platform defaults to the daemon host's, the transfer total is estimated from
// the source manifest, and the pull hook stamps retention.
func TestJobToEngine(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/riscv64"}
	w, js, up := engineCopier(t, eng)
	src := pushImage(t, up+"/team/app:1", 2)

	var hookEngine, hookRef string
	w.SetPullHook(func(engine, ref string) { hookEngine, hookRef = engine, ref })

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, created, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !created {
		t.Fatal("expected a new job")
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}

	if eng.pulled != "linux/riscv64" {
		t.Errorf("engine pulled platform %q, want the daemon host platform", eng.pulled)
	}
	if eng.ref != src.Name() {
		t.Errorf("engine pulled ref %q, want %q", eng.ref, src.Name())
	}
	tr := done.Transfers[0]
	if tr.Store != "node" || tr.Kind != "docker" || tr.Source != "up" {
		t.Errorf("transfer identity = %+v", tr)
	}
	if tr.State != "done" {
		t.Errorf("transfer state = %q", tr.State)
	}
	if tr.BytesTotal == 0 {
		t.Error("transfer total should be estimated from the source manifest")
	}
	if hookEngine != "node" || hookRef != eng.ref {
		t.Errorf("pull hook = (%q, %q), want (node, %q)", hookEngine, hookRef, eng.ref)
	}
	if len(done.Platforms) != 1 || done.Platforms[0] != "linux/riscv64" {
		t.Errorf("job platforms = %v, want the resolved engine platform", done.Platforms)
	}
}

// The daemon's own error (e.g. a platform the image lacks) fails the job as-is.
func TestJobToEngineDaemonErrorFailsJob(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64",
		pullErr: errors.New("no matching manifest for linux/s390x")}
	w, js, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node", Platforms: []string{"linux/s390x"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobFailed || !strings.Contains(done.Err, "no matching manifest") {
		t.Fatalf("state = %q err = %q, want the daemon error", done.State, done.Err)
	}
	if eng.pulled != "linux/s390x" {
		t.Errorf("requested platform must be passed through as-is, got %q", eng.pulled)
	}
}

func TestJobToEngineRejectsMultiplePlatforms(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)
	w.base = context.Background()

	_, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node",
		Platforms: []string{"linux/amd64", "linux/arm64"}})
	if err == nil || !strings.Contains(err.Error(), "single platform") {
		t.Fatalf("err = %v, want a single-platform rejection", err)
	}
}

func TestJobToEngineRejectsCopyReferrers(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)
	w.base = context.Background()

	yes := true
	_, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node", CopyReferrers: &yes})
	if err == nil || !strings.Contains(err.Error(), "registry destination") {
		t.Fatalf("err = %v, want a registry-destination rejection", err)
	}
}

// An unreachable daemon fails admission when the platform must be resolved from it.
func TestJobToEngineHostPlatformFailureFailsAdmission(t *testing.T) {
	eng := &fakePullEngine{name: "node"} // Platform() errors
	w, _, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)
	w.base = context.Background()

	if _, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"}); err == nil {
		t.Fatal("expected admission to fail when the daemon platform cannot be resolved")
	}
	// An explicit platform sidesteps the daemon probe entirely.
	res, err := w.Plan(context.Background(), Request{Ref: "team/app:1", Source: "up", Target: "node", Platforms: []string{"linux/arm64"}})
	if err != nil {
		t.Fatalf("explicit platform must not need the daemon: %v", err)
	}
	if len(res.Platforms) != 1 || res.Platforms[0] != "linux/arm64" {
		t.Errorf("plan platforms = %v", res.Platforms)
	}
}

// pull_host (engine) and downstream_host (source) rewrite the host the engine
// is told to pull from.
func TestEnginePullRefHostOverride(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	up := startRegistry(t)
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: up, Insecure: true, DownstreamHost: "cache.cr.example"},
	}, false)
	w.stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker", PullHost: "mirror.example"}, eng)
	pushImage(t, up+"/team/app:1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	// pull_host on the engine wins over the source's downstream_host.
	if want := "mirror.example/team/app:1"; eng.ref != want {
		t.Errorf("engine ref = %q, want %q (pull_host applied)", eng.ref, want)
	}
	_ = done
}

// A verified source pins the pull: the engine keeps the tag ref but is anchored
// to the verified digest.
func TestJobToEngineVerifiedDigestAnchor(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, up := engineCopier(t, eng)
	src := pushImage(t, up+"/team/app:1", 1)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("c", 64))
	w.SetVerifier(&fakeVerifier{dg: h})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if eng.ref != src.Name() {
		t.Errorf("engine ref = %q, want the tag ref %q", eng.ref, src.Name())
	}
	if eng.digest != h.String() {
		t.Errorf("engine anchor digest = %q, want the verified %q", eng.digest, h)
	}
	if done.Transfers[0].Digest != h.String() {
		t.Errorf("transfer digest = %q, want the verified digest", done.Transfers[0].Digest)
	}
}

// Daemon layer reports refine the upstream estimate (hybrid progress) — UPWARD.
//
// That is the only direction a report can be trusted in. A complete one is
// bigger than the registry's figure, which is a sum of layer sizes and counts
// no manifests. A report SMALLER than the estimate cannot be told apart from
// one that is merely incomplete, and down's poll makes incomplete reports out
// of fast pulls; engineSink.Layer says how.
func TestJobToEngineDaemonProgressRefinesEstimate(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64", reported: []down.LayerUpdate{
		{Digest: "l1", Total: 100_000, Done: 100_000, State: "done"},
		{Digest: "l2", Total: 400_000, Done: 400_000, State: "done"},
	}}
	w, js, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	tr := done.Transfers[0]
	if tr.BytesTotal != 500_000 {
		t.Errorf("total = %d, want the daemon-reported 500000 to supersede the estimate", tr.BytesTotal)
	}
	if got := tr.BytesDone; got != 500_000 {
		t.Errorf("done = %d, want 500000", got)
	}
	if len(tr.Layers) != 2 {
		t.Errorf("layers = %d, want the daemon-reported 2", len(tr.Layers))
	}
}

// A report smaller than the estimate does not lower it. This is the shape the
// fleet hit: the poll caught one blob of a pull that was over in less than a
// tick, and the sum of what it caught is not a smaller image, it is less of
// the same one.
func TestJobToEngineDoesNotLetASmallReportShrinkTheImage(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64", reported: []down.LayerUpdate{
		{Digest: "the-one-blob-the-poll-caught", Total: 26, Done: 26, State: "done"},
	}}
	w, js, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 4)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	tr := done.Transfers[0]
	if tr.BytesTotal <= 26 {
		t.Errorf("total = %d, which is what the poll happened to catch, not the image", tr.BytesTotal)
	}
	if got := tr.BytesDone; got != tr.BytesTotal {
		t.Errorf("done = %d, want the finished transfer to read full at %d", got, tr.BytesTotal)
	}
}

// A daemon on the containerd image store reports every layer with
// `progressDetail:{"hidecounts":true}` — names, no sizes. That is not a
// transfer of nothing, and the size the registry already told us is the whole
// of what anyone watching has. It survives the report.
func TestJobToEngineKeepsTheEstimateWhenTheDaemonCountsNothing(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64", reported: []down.LayerUpdate{
		{Digest: "l1", State: "done"},
		{Digest: "l2", State: "done"},
	}}
	w, js, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	tr := done.Transfers[0]
	if tr.BytesTotal <= 0 {
		t.Errorf("total = %d, want the upstream estimate to stand", tr.BytesTotal)
	}
	if got := tr.BytesDone; got != tr.BytesTotal {
		t.Errorf("done = %d, want the finished transfer to read full at %d", got, tr.BytesTotal)
	}
}

// The case that reached a fleet node: the daemon sizes SOME of what it pulled.
// On the containerd image store the image's own layers come back sizeless and
// something small alongside them — an attestation — comes back with a size, so
// the sum of what was sized is a number about a different thing. Reported 2026
// bytes for a 2.2 MB delivery, which reads like an answer and is not one.
func TestJobToEngineKeepsTheEstimateWhenTheDaemonSizesOnlySome(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64", reported: []down.LayerUpdate{
		{Digest: "the-image-layer", State: "done"},
		{Digest: "something-alongside", Total: 2026, Done: 2026, State: "done"},
	}}
	w, js, up := engineCopier(t, eng)
	// Eight layers: the thing alongside has to be SMALLER than the image,
	// the way an attestation is. `random.Image` makes 512-byte layers.
	pushImage(t, up+"/team/app:1", 8)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if got := done.Transfers[0].BytesTotal; got == 2026 {
		t.Errorf("total = %d, which is the one layer the daemon happened to size, not the image", got)
	} else if got <= 0 {
		t.Errorf("total = %d, want the upstream estimate to stand", got)
	}
}

// Order matters, and this is the order a node actually sees. The image's own
// layer is streamed through a pull-through cache that is still filling, so it
// arrives with no Content-Length and the daemon cannot size it; something small
// the cache already had — an attestation — arrives with one, and arrives FIRST.
//
// At that instant every layer the daemon has named is sized, so the report is
// complete and 2026 bytes is the honest total of it. One message later it is
// not, and the estimate has to come back.
func TestJobToEngineTakesTheEstimateBackWhenTheReportTurnsPartial(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64", reported: []down.LayerUpdate{
		{Digest: "something-alongside", Total: 2026, Done: 2026, State: "done"},
		{Digest: "the-image-layer", State: "done"},
	}}
	w, js, up := engineCopier(t, eng)
	// Eight layers: the thing alongside has to be SMALLER than the image,
	// the way an attestation is. `random.Image` makes 512-byte layers.
	pushImage(t, up+"/team/app:1", 8)

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if got := done.Transfers[0].BytesTotal; got == 2026 {
		t.Errorf("total = %d: the first report won and never gave the estimate back", got)
	} else if got <= 0 {
		t.Errorf("total = %d, want the upstream estimate", got)
	}
}

// Identical engine moves coalesce; a different engine is a different job.
func TestJobToEngineDedup(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)
	w.base = context.Background() // admit without workers so the first job stays active

	first, created, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil || !created {
		t.Fatalf("first submit: %v created=%v", err, created)
	}
	t.Cleanup(func() { js.Delete(first.ID) })
	second, created, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if created {
		t.Errorf("identical engine move should coalesce: created=%v", created)
	}
	if second.ID == first.ID {
		t.Errorf("coalesced submit should get its own handle, got %s twice", second.ID)
	}
}

// With no estimate there is no floor, and the daemon's report stands
// unchecked. That is a decision rather than an oversight: when the source
// manifest cannot be read there is nothing to compare against, a poll that
// caught one blob is indistinguishable from a complete report of a small
// image, and the alternative — reporting nothing — is the 0/0 that #21 was
// about. The number goes out and the missing estimate is logged at Warn, so
// the one case where it can be a plausible lie is visible.
func TestJobToEngineHasNoFloorWhenTheSourceCannotBeRead(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64", reported: []down.LayerUpdate{
		{Digest: "the-one-blob-the-poll-caught", Total: 2026, Done: 2026, State: "done"},
	}}
	w, js, _ := engineCopier(t, eng)
	// Deliberately never pushed: upstreamPlan has no manifest to size.

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/never-pushed:1", Source: "up", Target: "node"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	tr := done.Transfers[0]
	if tr.BytesTotal != 2026 {
		t.Errorf("total = %d, want the unchecked report of 2026 to stand", tr.BytesTotal)
	}
	if got := tr.BytesDone; got != tr.BytesTotal {
		t.Errorf("done = %d, want the finished transfer to read full at %d", got, tr.BytesTotal)
	}
}
