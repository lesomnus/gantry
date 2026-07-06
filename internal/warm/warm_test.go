package warm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/store"
)

func newWarmer(t *testing.T, stores []config.StoreConfig, allowUnknown bool) (*Warmer, Store) {
	t.Helper()
	m := make(map[string]config.StoreConfig, len(stores))
	for _, s := range stores {
		m[s.Name] = s
	}
	var c config.Config
	c.Stores = m
	c.Serve.AllowUnknownStores = allowUnknown
	c.Worker = config.WorkerConfig{MaxConcurrentJobs: 2, MaxConcurrentLayers: 2, QueueSize: 8}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	set, err := store.NewSet(c.Stores, c.Serve.AllowUnknownStores)
	if err != nil {
		t.Fatal(err)
	}
	js := NewMemStore()
	w := NewWarmer(set, js, c.Worker)
	w.srcOpts = []name.Option{name.Insecure}
	return w, js
}

func waitTerminal(t *testing.T, js Store, id string) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := js.Snapshot(id); ok && snap.State.Terminal() {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting for terminal state")
	return JobSnapshot{}
}

func TestWarmerCopyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	up := startRegistry(t)
	cache := startRegistry(t)
	pushImage(t, up+"/team/app:1", 3)

	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: up, Insecure: true},
		{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"},
	}, false)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", From: "up", To: "cache", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	done := waitTerminal(t, js, snap.ID)
	if done.State != JobDone {
		t.Fatalf("state = %q (err=%q)", done.State, done.Err)
	}
	if len(done.Transfers) != 1 {
		t.Fatalf("transfers = %d, want 1", len(done.Transfers))
	}
	tr := done.Transfers[0]
	if tr.Store != "cache" || tr.From != "up" || tr.Kind != "oci" {
		t.Errorf("transfer = %+v", tr)
	}
	if tr.BytesTotal == 0 || tr.BytesDone != tr.BytesTotal {
		t.Errorf("bytes = %d/%d", tr.BytesDone, tr.BytesTotal)
	}
	dst, err := name.ParseReference(tr.Ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(dst); err != nil {
		t.Errorf("cache ref %q not resolvable: %v", tr.Ref, err)
	}
}

func TestWarmerDedup(t *testing.T) {
	w, _ := newWarmer(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background() // enqueue without starting workers; jobs stay active
	req := Request{Ref: "team/app:1", From: "docker.io", To: "cache", Platforms: []string{"linux/amd64"}}
	s1, _, err := w.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	s2, _, err := w.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != s2.ID {
		t.Errorf("expected dedup onto one job, got %s and %s", s1.ID, s2.ID)
	}
}

func TestWarmerQueueFull(t *testing.T) {
	w, _ := newWarmer(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background()
	w.jobs = make(chan *Job, 1)
	if _, _, err := w.Submit(Request{Ref: "a/x:1", From: "r.io", To: "cache"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := w.Submit(Request{Ref: "b/y:1", From: "r.io", To: "cache"})
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestWarmerNothingToDo(t *testing.T) {
	w, _ := newWarmer(t, nil, true)
	w.base = context.Background()
	if _, _, err := w.Submit(Request{Ref: "x/y:1", From: "r.io"}); err == nil {
		t.Error("expected error when `to` is not set")
	}
}

func TestWarmerSweepsTerminalJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w, js := newWarmer(t, nil, true)
	w.wc.JobTTL = config.Duration(50 * time.Millisecond)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	done_job := NewJob("job_done", "a/b:1", nil, time.Now())
	done_job.State = JobDone
	done_job.EndedAt = time.Now()
	live_job := NewJob("job_live", "a/b:2", nil, time.Now())
	for _, j := range []*Job{done_job, live_job} {
		if err := js.Add(j); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := js.Snapshot("job_done"); !ok {
			if _, ok := js.Snapshot("job_live"); !ok {
				t.Fatal("non-terminal job was swept")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("terminal job was not swept after its TTL")
}

// failSource fails the first layer with a real error; other layers block until
// the failure's self-cancel aborts them, like in-flight sibling copies.
type failSource struct{}

func (failSource) Resolve(context.Context, name.Reference, name.Reference, []string) (*Plan, error) {
	return nil, nil
}

func (failSource) Warm(ctx context.Context, _, _ name.Repository, l PlannedLayer, _ ProgressSink) error {
	if l.Digest == "sha256:1" {
		return errors.New("copy blob: boom")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (failSource) Commit(context.Context, name.Reference, name.Reference, []string, bool) (v1.Hash, error) {
	return v1.Hash{}, nil
}

func TestCopyLayersReportsLayerError(t *testing.T) {
	w, js := newWarmer(t, nil, true)
	w.wc.MaxConcurrentLayers = 1
	ctx, cancel := context.WithCancel(context.Background())
	job := NewJob("job_l", "a/b:1", nil, time.Now())
	job.ctx, job.cancel = ctx, cancel
	tr := &Transfer{Layers: []*LayerProgress{{}, {}, {}}}
	job.Transfers = []*Transfer{tr}
	if err := js.Add(job); err != nil {
		t.Fatal(err)
	}
	src_ref, _ := name.ParseReference("up.local/a/b:1", name.Insecure)
	dst_ref, _ := name.ParseReference("cache.local/a/b:1", name.Insecure)
	ex := &jobExec{src: src_ref, cacheRef: dst_ref}
	plan := &Plan{Layers: []PlannedLayer{{Digest: "sha256:1"}, {Digest: "sha256:2"}, {Digest: "sha256:3"}}}

	// The failing layer self-cancels the job ctx while the dispatcher is still
	// admitting layers; the returned error must be the layer error either way.
	err := w.copyLayers(job.ctx, job, tr, failSource{}, plan, ex)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the layer error", err)
	}
	w.finish(job, err)
	snap, _ := js.Snapshot("job_l")
	if snap.State != JobFailed || snap.Err != "copy blob: boom" {
		t.Fatalf("state = %q err = %q, want failed with the layer error", snap.State, snap.Err)
	}
}

func TestFinish(t *testing.T) {
	t.Run("layer failure self-cancel is recorded as failed", func(t *testing.T) {
		w, js := newWarmer(t, nil, true)
		ctx, cancel := context.WithCancel(context.Background())
		job := NewJob("job_f", "a/b:1", nil, time.Now())
		job.ctx, job.cancel = ctx, cancel
		if err := js.Add(job); err != nil {
			t.Fatal(err)
		}
		// A failing layer cancels the job context to abort its siblings before
		// the error propagates; the real error must win over that cancellation.
		job.Cancel()
		w.finish(job, errors.New("copy blob: boom"))
		snap, _ := js.Snapshot("job_f")
		if snap.State != JobFailed {
			t.Fatalf("state = %q, want %q", snap.State, JobFailed)
		}
		if snap.Err != "copy blob: boom" {
			t.Fatalf("err = %q, want the layer error", snap.Err)
		}
	})
	t.Run("cancellation stays canceled", func(t *testing.T) {
		w, js := newWarmer(t, nil, true)
		ctx, cancel := context.WithCancel(context.Background())
		job := NewJob("job_c", "a/b:1", nil, time.Now())
		job.ctx, job.cancel = ctx, cancel
		if err := js.Add(job); err != nil {
			t.Fatal(err)
		}
		job.Cancel()
		w.finish(job, context.Canceled)
		snap, _ := js.Snapshot("job_c")
		if snap.State != JobCanceled {
			t.Fatalf("state = %q, want %q", snap.State, JobCanceled)
		}
	})
}

func TestActiveSkipsCanceledJob(t *testing.T) {
	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background()
	req := Request{Ref: "team/app:1", From: "docker.io", To: "cache", Platforms: []string{"linux/amd64"}}
	s1, _, err := w.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	// Cancel (without evicting): a resubmit must start a fresh job, not coalesce
	// onto the dying one.
	job, _ := js.Job(s1.ID)
	job.Cancel()
	s2, created, err := w.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	if !created || s2.ID == s1.ID {
		t.Errorf("resubmit after cancel coalesced onto %s (created=%v)", s1.ID, created)
	}
}

func TestRetryRequiresTerminal(t *testing.T) {
	w, js := newWarmer(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background()
	s1, _, err := w.Submit(Request{Ref: "team/app:1", From: "docker.io", To: "cache", Platforms: []string{"linux/amd64"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Retry(s1.ID); !errors.Is(err, ErrJobActive) {
		t.Errorf("retry of a pending job err = %v, want ErrJobActive", err)
	}
	if _, _, err := w.Retry("job_missing"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("retry of a missing job err = %v, want ErrJobNotFound", err)
	}
	js.Update(s1.ID, func(j *Job) { j.State = JobFailed })
	s2, created, err := w.Retry(s1.ID)
	if err != nil || !created || s2.ID == s1.ID {
		t.Errorf("retry = (%s, %v, %v), want a fresh job", s2.ID, created, err)
	}
}
