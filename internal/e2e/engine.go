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
	inUse    map[string]bool
	held     map[string]string             // ref -> anchor digest ("" if none)
	untagged map[string]down.UntaggedImage // id -> untagged image
	pulls    []pullRecord
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
	if e.pullErr != nil {
		return nil, e.pullErr
	}
	e.pulls = append(e.pulls, pullRecord{Ref: ref, Digest: digest, Platform: platform, As: append([]string(nil), as...)})
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

// --- test controls (hold the lock) ---

func (e *fakeEngine) setReady(err error)      { e.mu.Lock(); e.ready = err; e.mu.Unlock() }
func (e *fakeEngine) setInUse(refs ...string) { e.mu.Lock(); for _, r := range refs { e.inUse[r] = true }; e.mu.Unlock() }
func (e *fakeEngine) pullCount() int          { e.mu.Lock(); defer e.mu.Unlock(); return len(e.pulls) }
func (e *fakeEngine) lastPull() pullRecord    { e.mu.Lock(); defer e.mu.Unlock(); return e.pulls[len(e.pulls)-1] }
func (e *fakeEngine) has(ref string) bool     { e.mu.Lock(); defer e.mu.Unlock(); _, ok := e.held[ref]; return ok }
