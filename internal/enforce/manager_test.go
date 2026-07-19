package enforce

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/verify"
)

// --- test doubles ---

type fakeEngine struct {
	name       string
	img        down.ContainerImage
	resolveErr error

	mu             sync.Mutex
	removedConts   []string
	removedImgs    []string
	running        []down.StartEvent // ListRunning result (cold-reconcile tests)
	watchErr       chan error        // WatchStarts drains a queued error before blocking
	watchCalls     int
	removeContErr  error // RemoveContainer returns this (failure-branch tests)
	removeImgErr   error // Remove returns this
	listRunningErr error // ListRunning returns this
}

func (f *fakeEngine) Name() string { return f.name }
func (f *fakeEngine) Kind() string { return "docker" }
func (f *fakeEngine) WatchStarts(ctx context.Context, _ func(down.StartEvent)) error {
	f.mu.Lock()
	f.watchCalls++
	ch := f.watchErr
	f.mu.Unlock()
	if ch != nil {
		select {
		case err := <-ch:
			return err // simulate a dropped stream so the manager reconnects
		default:
		}
	}
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeEngine) ListRunning(context.Context) ([]down.StartEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, f.listRunningErr
}
func (f *fakeEngine) ResolveImage(context.Context, string) (down.ContainerImage, error) {
	return f.img, f.resolveErr
}
func (f *fakeEngine) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeContErr != nil {
		return f.removeContErr
	}
	f.removedConts = append(f.removedConts, id)
	return nil
}
func (f *fakeEngine) Remove(_ context.Context, ref string) (down.RemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedImgs = append(f.removedImgs, ref)
	return down.RemoveResult{}, f.removeImgErr
}

func (f *fakeEngine) contRemovals() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removedConts)
}
func (f *fakeEngine) imgRemovals() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removedImgs)
}
func (f *fakeEngine) removedContainer(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.removedConts {
		if c == id {
			return true
		}
	}
	return false
}
func (f *fakeEngine) setRunning(evs ...down.StartEvent) {
	f.mu.Lock()
	f.running = evs
	f.mu.Unlock()
}

type fakeVerifier struct {
	fn    func() (verify.Result, error)
	calls int
}

func (v *fakeVerifier) Verify(context.Context, config.StoreConfig, name.Reference) (verify.Result, error) {
	v.calls++
	return v.fn()
}
func (v *fakeVerifier) Describe() verify.Description        { return verify.Description{} }
func (v *fakeVerifier) Reload() (verify.Description, error) { return verify.Description{}, nil }

const testHost = "reg.test"

func testDigest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func repoDigest(c string) string { return testHost + "/app@" + testDigest(c) }

func trustedResult() (verify.Result, error) {
	h, _ := v1.NewHash(testDigest("a"))
	return verify.Result{Mode: config.VerifyRequire, Digest: h}, nil
}

// harness wires a Manager over a fake engine with a real (temp) cache.
type harness struct {
	m      *Manager
	eng    *fakeEngine
	cache  *verify.Cache
	verify *fakeVerifier
	now    *time.Time
}

func newHarness(t *testing.T, policy string, vfn func() (verify.Result, error)) *harness {
	t.Helper()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cache, err := verify.OpenCache(filepath.Join(t.TempDir(), "v.db"), 28*24*time.Hour, 14*24*time.Hour,
		verify.WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	eng := &fakeEngine{name: "dockerd", img: down.ContainerImage{ImageID: "sha256:img", RepoDigests: []string{repoDigest("a")}}}
	vf := &fakeVerifier{fn: vfn}
	stores := map[string]config.StoreConfig{"local": {Name: "local", Kind: "oci", Host: testHost, Insecure: true}}
	m := NewManager([]Store{{Name: "dockerd", Engine: eng}}, cache, vf, stores, Options{
		OnUnavailable: policy,
		Now:           func() time.Time { return now },
	})
	return &harness{m: m, eng: eng, cache: cache, verify: vf, now: &now}
}

func (h *harness) handle(id string) {
	h.m.handle(context.Background(), h.eng, down.StartEvent{ContainerID: id, Image: testHost + "/app:1"})
}

func killed(h *harness) bool { return h.eng.contRemovals() > 0 }

// --- tests ---

func TestFreshTrustedCacheAllows(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) {
		t.Fatal("verifier must not be called on a fresh cache hit")
		return verify.Result{}, nil
	})
	_ = h.cache.Put(testDigest("a"), true, config.VerifyRequire, "")
	h.handle("c1")
	if killed(h) {
		t.Error("trusted cache hit must not be quarantined")
	}
}

func TestFreshUntrustedCacheKills(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) {
		t.Fatal("verifier must not be called on a fresh cache hit")
		return verify.Result{}, nil
	})
	_ = h.cache.Put(testDigest("a"), false, config.VerifyRequire, "")
	h.handle("c1")
	if !killed(h) {
		t.Error("untrusted cache hit must be quarantined")
	}
	if h.eng.imgRemovals() == 0 {
		t.Error("image should be removed after the kill")
	}
}

func TestMissLiveTrustedAllows(t *testing.T) {
	h := newHarness(t, "grace", trustedResult)
	h.handle("c1")
	if killed(h) {
		t.Error("live-verified trusted image must not be quarantined")
	}
	if h.verify.calls != 1 {
		t.Errorf("expected 1 live verify, got %d", h.verify.calls)
	}
}

func TestMissLiveUntrustedKills(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) { return verify.Result{}, verify.ErrUnsigned })
	h.handle("c1")
	if !killed(h) {
		t.Error("live unsigned image must be quarantined")
	}
}

func TestUnavailableGraceHonorsExpiredTrusted(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) {
		return verify.Result{}, errors.New("dial tcp: connection refused")
	})
	_ = h.cache.Put(testDigest("a"), true, config.VerifyRequire, "")
	*h.now = h.now.Add(29 * 24 * time.Hour) // expire the verdict past hard TTL
	h.handle("c1")
	if killed(h) {
		t.Error("grace must honor an expired-but-trusted verdict during an outage")
	}
}

func TestUnavailableGraceNoVerdictAllows(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) {
		return verify.Result{}, errors.New("dial tcp: connection refused")
	})
	h.handle("c1")
	if killed(h) {
		t.Error("grace with no verdict should allow (log), not kill")
	}
}

func TestUnavailableKillPolicyKills(t *testing.T) {
	h := newHarness(t, "kill", func() (verify.Result, error) {
		return verify.Result{}, errors.New("dial tcp: connection refused")
	})
	h.handle("c1")
	if !killed(h) {
		t.Error("on_unavailable=kill must fail closed")
	}
}

func TestNoProvenanceGraceAllows(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) { t.Fatal("no digest -> no verify"); return verify.Result{}, nil })
	h.eng.img = down.ContainerImage{ImageID: "sha256:local", RepoDigests: nil} // locally built
	h.handle("c1")
	if killed(h) {
		t.Error("an image with no registry provenance should not be killed under grace")
	}
}

func TestSelfContainerNeverKilled(t *testing.T) {
	h := newHarness(t, "kill", func() (verify.Result, error) { return verify.Result{}, verify.ErrUnsigned })
	h.m.self = selfGuard{id: "abc123"}
	h.m.handle(context.Background(), h.eng, down.StartEvent{ContainerID: "abc123def456", Image: "x"})
	if killed(h) {
		t.Error("gantry's own container must never be quarantined")
	}
}

func TestResolveFailureAllows(t *testing.T) {
	h := newHarness(t, "kill", func() (verify.Result, error) { return verify.Result{}, nil })
	h.eng.resolveErr = errors.New("no such container")
	h.handle("gone")
	if killed(h) {
		t.Error("an inspect failure must never trigger a kill")
	}
}

func TestIdempotentReplay(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) { return verify.Result{}, verify.ErrUnsigned })
	h.handle("c1")
	h.handle("c1") // replayed event
	// both removals are attempted; RemoveContainer is a no-op on a gone container
	if h.eng.contRemovals() != 2 {
		t.Errorf("replay should re-attempt removal idempotently, got %d", len(h.eng.removedConts))
	}
}

func TestTopLevelDigestPrefersRepoDigest(t *testing.T) {
	ci := down.ContainerImage{RepoDigests: []string{repoDigest("b")}, ManifestDigest: testDigest("f")}
	if got := topLevelDigest(ci); got != testDigest("b") {
		t.Errorf("topLevelDigest = %s, want the RepoDigest not the ManifestDigest", got)
	}
	if got := topLevelDigest(down.ContainerImage{ManifestDigest: testDigest("f")}); got != "" {
		t.Errorf("no RepoDigest should yield empty digest, got %s", got)
	}
}

func TestImageToRemoveUsesDigest(t *testing.T) {
	dg := testDigest("a")
	cases := []struct {
		image string
		want  string
	}{
		{"reg.test/app:1", "reg.test/app@" + dg},                  // tag stripped
		{"reg.test:5000/app", "reg.test:5000/app@" + dg},          // host:port, no tag
		{"reg.test:5000/app:1", "reg.test:5000/app@" + dg},        // host:port + tag
		{"reg.test/app@" + testDigest("b"), "reg.test/app@" + dg}, // digest-form: no double @
		{"alpine:latest", "alpine@" + dg},                         // no host, tag
		{"alpine", "alpine@" + dg},                                // bare name
		{testDigest("f"), testDigest("f")},                        // bare image id: unchanged
	}
	for _, tc := range cases {
		if got := imageToRemove(down.StartEvent{Image: tc.image}, decision{digest: dg}); got != tc.want {
			t.Errorf("imageToRemove(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
	// no digest -> the event image verbatim
	if got := imageToRemove(down.StartEvent{Image: "alpine:1"}, decision{}); got != "alpine:1" {
		t.Errorf("imageToRemove(no digest) = %q", got)
	}
}

// --- policy, mapping, and quarantine-failure branches ---

func TestOnUnavailableAllowPolicy(t *testing.T) {
	h := newHarness(t, "allow", func() (verify.Result, error) {
		return verify.Result{}, errors.New("dial tcp: connection refused")
	})
	h.handle("c1")
	if killed(h) {
		t.Error("on_unavailable=allow must never quarantine on an unobtainable verdict")
	}
}

func TestNoMatchingSourceStore(t *testing.T) {
	// A RepoDigest host that matches no configured oci store: no live verify runs,
	// and the decision falls to the policy.
	h := newHarness(t, "kill", func() (verify.Result, error) {
		return verify.Result{}, errors.New("should not be called")
	})
	h.eng.img = down.ContainerImage{RepoDigests: []string{"unknown.host/app@" + testDigest("a")}}
	h.handle("c1")
	if !killed(h) {
		t.Error("no matching source store under kill policy should quarantine")
	}
	if h.verify.calls != 0 {
		t.Errorf("live verify must not run without a matching store, calls=%d", h.verify.calls)
	}
}

func TestQuarantineRemoveContainerErrorSkipsImage(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) { return verify.Result{}, verify.ErrUnsigned })
	h.eng.removeContErr = errors.New("daemon busy")
	h.handle("c1") // must not panic
	if h.eng.imgRemovals() != 0 {
		t.Error("image removal must be skipped when container removal fails")
	}
}

func TestQuarantineImageRemoveErrorTolerated(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) { return verify.Result{}, verify.ErrUnsigned })
	h.eng.removeImgErr = errors.New("conflict: image is referenced by another container")
	h.handle("c1") // image-remove conflict must be swallowed
	if !killed(h) {
		t.Error("container should still be removed")
	}
	if h.eng.imgRemovals() != 1 {
		t.Error("image removal should have been attempted")
	}
}

// --- watch loop: cold reconcile, reconnect, list-error ---

func waitRemoval(h *harness, id string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if h.eng.removedContainer(id) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h.eng.removedContainer(id)
}

func TestColdReconcileQuarantines(t *testing.T) {
	// A trusted verifier would allow, so a pre-running container that IS removed
	// proves the cold reconcile decided from the untrusted cache verdict.
	h := newHarness(t, "grace", trustedResult)
	_ = h.cache.Put(testDigest("a"), false, config.VerifyRequire, "")
	h.eng.setRunning(down.StartEvent{ContainerID: "pre", Image: testHost + "/app:1"})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); h.m.Stop() }()
	h.m.StartWatchers(ctx)

	if !waitRemoval(h, "pre", 2*time.Second) {
		t.Fatal("a pre-running untrusted container was not quarantined by the cold reconcile")
	}
}

func TestWatchReconnects(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) { return verify.Result{}, verify.ErrUnsigned })
	h.eng.watchErr = make(chan error, 1)
	h.eng.watchErr <- errors.New("event stream dropped")

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); h.m.Stop() }()
	h.m.StartWatchers(ctx)

	// After the first WatchStarts errors, the manager backs off (~2s) and
	// re-reconciles. Register a running container so the reconnect reconcile
	// quarantines it (live verify -> unsigned).
	time.Sleep(100 * time.Millisecond)
	h.eng.setRunning(down.StartEvent{ContainerID: "late", Image: testHost + "/app:1"})

	if !waitRemoval(h, "late", 5*time.Second) {
		t.Fatal("reconnect reconcile did not quarantine the container")
	}
	h.eng.mu.Lock()
	calls := h.eng.watchCalls
	h.eng.mu.Unlock()
	if calls < 2 {
		t.Errorf("expected a reconnect (>=2 WatchStarts calls), got %d", calls)
	}
}

func TestReconcileListErrorDoesNotQuarantine(t *testing.T) {
	h := newHarness(t, "grace", func() (verify.Result, error) { return verify.Result{}, nil })
	h.eng.listRunningErr = errors.New("daemon unreachable")
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); h.m.Stop() }()
	h.m.StartWatchers(ctx) // reconcile logs the error and returns; no panic
	time.Sleep(50 * time.Millisecond)
	if killed(h) {
		t.Error("a failed reconcile must not quarantine anything")
	}
}
