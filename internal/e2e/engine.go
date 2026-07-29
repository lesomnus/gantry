package e2e

import (
	"context"
	"sync"

	"github.com/lesomnus/gantry/internal/down"
)

// fakeEngine is an in-memory down.Engine for the hermetic tier. It records every
// Pull, holds the reference set the "daemon" would have, and lets a test toggle
// readiness, in-use, and pull failure. It implements down.Reconciler so the
// untagged reaper is exercisable without a real daemon.
type fakeEngine struct {
	name string

	mu       sync.Mutex
	kind     string
	platform string
	ready    error
	pullErr  error
	// failFor fails a pull whose reference matches, so one source can be broken
	// while another serves the same image (a fallback's whole point). Consulted
	// after pullErr.
	failFor  func(ref string) error
	inUse    map[string]bool
	held     map[string]string             // ref -> anchor digest ("" if none)
	untagged map[string]down.UntaggedImage // id -> untagged image
	pulls    []pullRecord

	// down.Enforcer (runtime enforcement) state.
	starts  chan down.StartEvent           // injected container-start events
	running map[string]down.ContainerImage // container id -> its resolved image
	removed map[string]bool                // container ids force-removed by enforcement
}

type pullRecord struct {
	Ref, Digest, Platform string
	As                    []string
}

func newFakeEngine(name string) *fakeEngine {
	return &fakeEngine{
		name:     name,
		kind:     "docker",
		platform: "linux/amd64",
		inUse:    map[string]bool{},
		held:     map[string]string{},
		untagged: map[string]down.UntaggedImage{},
		starts:   make(chan down.StartEvent, 64),
		running:  map[string]down.ContainerImage{},
		removed:  map[string]bool{},
	}
}

func (e *fakeEngine) Name() string { return e.name }
func (e *fakeEngine) Kind() string { return e.kind }

func (e *fakeEngine) Ready(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}

func (e *fakeEngine) Platform(context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.platform, nil
}

func (e *fakeEngine) Pull(_ context.Context, ref, digest, platform string, as []string, _ *down.AnchorBlob, sink down.Sink) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Recorded before any failure is applied: pullRecords is the history of what
	// the engine was ASKED to pull, which is what a fallback test reasons about.
	e.pulls = append(e.pulls, pullRecord{Ref: ref, Digest: digest, Platform: platform, As: append([]string(nil), as...)})
	if e.pullErr != nil {
		return nil, e.pullErr
	}
	if e.failFor != nil {
		if err := e.failFor(ref); err != nil {
			return nil, err
		}
	}
	if sink != nil {
		sink.Layer(down.LayerUpdate{Digest: "sha256:" + ref, Total: 1, Done: 1, State: "done"})
	}
	recorded := as
	if len(recorded) == 0 {
		recorded = []string{ref}
	}
	for _, r := range recorded {
		e.held[r] = digest
	}
	return append([]string(nil), recorded...), nil
}

func (e *fakeEngine) InUse(context.Context) (map[string]bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]bool, len(e.inUse))
	for k, v := range e.inUse {
		out[k] = v
	}
	return out, nil
}

func (e *fakeEngine) SeedUsage(context.Context, down.UsageSink) error { return nil }

func (e *fakeEngine) WatchUsage(ctx context.Context, _ down.UsageSink) error {
	<-ctx.Done()
	return ctx.Err()
}

func (e *fakeEngine) Remove(_ context.Context, ref string) (down.RemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.held[ref]; ok {
		delete(e.held, ref)
		return down.RemoveResult{Untagged: []string{ref}}, nil
	}
	return down.RemoveResult{}, nil
}

func (e *fakeEngine) Close() error { return nil }

// --- down.Reconciler ---

func (e *fakeEngine) Images(context.Context) (down.Inventory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var inv down.Inventory
	for r := range e.held {
		inv.Refs = append(inv.Refs, r)
	}
	for _, u := range e.untagged {
		inv.Untagged = append(inv.Untagged, u)
	}
	return inv, nil
}

func (e *fakeEngine) ReapUntagged(_ context.Context, id string, owned func(string) bool) (down.RemoveResult, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	u, ok := e.untagged[id]
	if !ok {
		return down.RemoveResult{}, true, nil // already gone
	}
	for _, rd := range u.RepoDigests {
		if owned != nil && owned(rd) {
			return down.RemoveResult{}, false, nil // caller's index claimed it
		}
	}
	delete(e.untagged, id)
	return down.RemoveResult{Deleted: []string{id}}, true, nil
}

// --- down.Enforcer (runtime enforcement) ---

func (e *fakeEngine) WatchStarts(ctx context.Context, sink func(down.StartEvent)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-e.starts:
			sink(ev)
		}
	}
}

func (e *fakeEngine) ListRunning(context.Context) ([]down.StartEvent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]down.StartEvent, 0, len(e.running))
	for id, img := range e.running {
		out = append(out, down.StartEvent{ContainerID: id, Image: img.ConfigImage})
	}
	return out, nil
}

func (e *fakeEngine) ResolveImage(_ context.Context, id string) (down.ContainerImage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id], nil // zero value (no digest) when unknown/removed
}

func (e *fakeEngine) RemoveContainer(_ context.Context, id string, _ bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removed[id] = true
	delete(e.running, id)
	return nil
}

// --- test controls (hold the lock) ---

// startContainer registers a running container and injects its start event, so
// the enforcement watcher resolves it to repoDigests and decides a verdict.
func (e *fakeEngine) startContainer(id, image string, repoDigests ...string) {
	e.mu.Lock()
	e.running[id] = down.ContainerImage{ConfigImage: image, ImageID: "sha256:" + id, RepoDigests: repoDigests}
	e.mu.Unlock()
	e.starts <- down.StartEvent{ContainerID: id, Image: image}
}

func (e *fakeEngine) wasRemoved(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.removed[id]
}

func (e *fakeEngine) setReady(err error) { e.mu.Lock(); e.ready = err; e.mu.Unlock() }
func (e *fakeEngine) setInUse(refs ...string) {
	e.mu.Lock()
	for _, r := range refs {
		e.inUse[r] = true
	}
	e.mu.Unlock()
}
func (e *fakeEngine) pullCount() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.pulls) }

// pullRecords returns every pull the engine was asked for, in order — a job
// with a fallback source makes more than one.
func (e *fakeEngine) pullRecords() []pullRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]pullRecord(nil), e.pulls...)
}
func (e *fakeEngine) lastPull() pullRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pulls[len(e.pulls)-1]
}
func (e *fakeEngine) has(ref string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.held[ref]
	return ok
}
