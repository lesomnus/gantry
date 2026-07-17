package cpx

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// A job with `as` records the image under those names: the engine receives
// them verbatim, the retention hook stamps each one, and the snapshot carries
// them.
func TestJobToEngineAs(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, js, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)

	var mu sync.Mutex
	var stamped []string
	w.SetPullHook(func(_, ref string) {
		mu.Lock()
		stamped = append(stamped, ref)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	as := []string{"docker.io/team/app:1", "legacy.io/team/app:stable"}
	snap, created, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node", As: as})
	if err != nil || !created {
		t.Fatalf("submit: created=%v err=%v", created, err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}

	// Names reach the engine verbatim — no docker.io -> index.docker.io
	// normalization, since containerd resolves names by exact match.
	eng.mu.Lock()
	got := eng.as
	eng.mu.Unlock()
	if len(got) != 2 || got[0] != as[0] || got[1] != as[1] {
		t.Errorf("engine as = %v, want %v", got, as)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stamped) != 2 || stamped[0] != as[0] || stamped[1] != as[1] {
		t.Errorf("retention stamps = %v, want %v", stamped, as)
	}
	if len(done.As) != 2 || done.As[0] != as[0] {
		t.Errorf("snapshot as = %v", done.As)
	}
}

func TestAsValidation(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)
	w.SetBaseContext(context.Background()) // admission only; no workers

	// A registry destination has no naming step.
	_, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "up", As: []string{"docker.io/team/app:1"}})
	if err == nil || !strings.Contains(err.Error(), "registry") {
		t.Errorf("registry dest must reject as, got %v", err)
	}

	// A digest `as` needs a digest-pinned job: on an unanchored tag job there
	// is nothing that says what the name would mean.
	_, _, err = w.Submit(Request{
		Ref: "team/app:1", Source: "up", Target: "node",
		As: []string{"docker.io/team/app@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
	})
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("digest as on an unanchored job must be rejected, got %v", err)
	}
}

// Jobs that differ only in `as` are different moves and must not coalesce.
func TestAsDedup(t *testing.T) {
	eng := &fakePullEngine{name: "node", platform: "linux/amd64"}
	w, _, up := engineCopier(t, eng)
	pushImage(t, up+"/team/app:1", 1)
	w.SetBaseContext(context.Background()) // jobs stay pending: dedup is observable

	first, created, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node", As: []string{"a.example.com/team/app:1"}})
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	same, created, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node", As: []string{"a.example.com/team/app:1"}})
	if err != nil || created {
		t.Fatalf("identical submit must coalesce: created=%v id=%s err=%v", created, same.ID, err)
	}
	if same.ID == first.ID {
		t.Fatalf("coalesced submit should get its own handle, got %s twice", same.ID)
	}
	other, created, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "node", As: []string{"b.example.com/team/app:1"}})
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("different as must not coalesce: created=%v err=%v", created, err)
	}
}
