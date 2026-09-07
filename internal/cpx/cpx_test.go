package cpx

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/store"
)

func newCopier(t *testing.T, stores []config.StoreConfig, allowUnknown bool) (*Copier, Store) {
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
	w := NewCopier(set, js, c.Worker)
	w.srcOpts = []name.Option{name.Insecure}
	// Re-attempt delays are real seconds in production; tests assert what the
	// attempts do, not how long they wait for each other.
	w.backoff = func(int) time.Duration { return time.Millisecond }
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

func TestCopierCopyEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	up := startRegistry(t)
	cache := startRegistry(t)
	pushImage(t, up+"/team/app:1", 3)

	w, js := newCopier(t, []config.StoreConfig{
		{Name: "up", Kind: "oci", Host: up, Insecure: true},
		{Name: "cache", Kind: "oci", Host: cache, Insecure: true, Mode: "copy"},
	}, false)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	snap, _, err := w.Submit(Request{Ref: "team/app:1", Source: "up", Target: "cache", Platforms: []string{"linux/amd64"}})
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
	if tr.Store != "cache" || tr.Source != "up" || tr.Kind != "oci" {
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

func TestCopierDedup(t *testing.T) {
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background() // enqueue without starting workers; jobs stay active
	req := Request{Ref: "team/app:1", Source: "docker.io", Target: "cache", Platforms: []string{"linux/amd64"}, Labels: map[string]string{"team": "a"}}
	s1, c1, err := w.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	// An identical submit coalesces onto the running move but still gets its own
	// handle, so its id and its labels stay independent of the first caller's.
	req2 := req
	req2.Labels = map[string]string{"team": "b"}
	s2, c2, err := w.Submit(req2)
	if err != nil {
		t.Fatal(err)
	}
	if !c1 || c2 {
		t.Errorf("created flags = %v, %v; want true, false (second coalesces)", c1, c2)
	}
	if s1.ID == s2.ID {
		t.Errorf("coalesced submit should get its own handle, got %s twice", s1.ID)
	}
	if s1.Labels["team"] != "a" || s2.Labels["team"] != "b" {
		t.Errorf("handles must keep their own labels, got %v and %v", s1.Labels, s2.Labels)
	}
	// The two handles share a single execution.
	total := 0
	for _, c := range js.Counts() {
		total += c
	}
	if total != 1 {
		t.Errorf("executions = %d, want 1 shared job", total)
	}
	if got := js.List(Filter{}); len(got) != 2 {
		t.Errorf("list = %d handles, want 2", len(got))
	}
}

func TestRetryHonorsHandleViewAndLabels(t *testing.T) {
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background() // no workers: the shared job stays active
	reqA := Request{Ref: "team/app:1", Source: "docker.io", Target: "cache", Platforms: []string{"linux/amd64"}, Labels: map[string]string{"who": "a"}}
	sA, _, err := w.Submit(reqA)
	if err != nil {
		t.Fatal(err)
	}
	reqB := reqA
	reqB.Labels = map[string]string{"who": "b"}
	sB, coalesced, err := w.Submit(reqB)
	if err != nil {
		t.Fatal(err)
	}
	if coalesced {
		t.Fatal("second submit should coalesce onto the first")
	}

	// B cancels its handle; A's execution keeps running, so from B's side the
	// job now reads terminal (canceled) while the execution is still active.
	if _, ok, already := js.Cancel(sB.ID); !ok || already {
		t.Fatalf("cancel B: ok=%v already=%v", ok, already)
	}

	// Retry must honor B's own terminal view (not the still-running execution)
	// and resubmit under B's own labels (not A's).
	rB, _, err := w.Retry(sB.ID)
	if err != nil {
		t.Fatalf("retry of a canceled handle should be allowed, got %v", err)
	}
	if rB.ID == sB.ID {
		t.Error("retry should mint a fresh handle")
	}
	if rB.Labels["who"] != "b" {
		t.Errorf("retry labels = %v, want the caller's own who=b", rB.Labels)
	}
	_ = sA
}

func TestCopierQueueFull(t *testing.T) {
	w, _ := newCopier(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background()
	w.jobs = make(chan *Job, 1)
	if _, _, err := w.Submit(Request{Ref: "a/x:1", Source: "r.io", Target: "cache"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := w.Submit(Request{Ref: "b/y:1", Source: "r.io", Target: "cache"})
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestCopierNothingToDo(t *testing.T) {
	w, _ := newCopier(t, nil, true)
	w.base = context.Background()
	if _, _, err := w.Submit(Request{Ref: "x/y:1", Source: "r.io"}); err == nil {
		t.Error("expected error when `target` is not set")
	}
}

func TestCopierSweepsTerminalJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w, js := newCopier(t, nil, true)
	w.wc.JobTTL = config.Duration(50 * time.Millisecond)
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Stop() })

	done_job := NewJob("job_done", "a/b:1", nil, time.Now())
	done_job.State = JobDone
	done_job.DateEnded = time.Now()
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
// the failure's abort reaches them, like in-flight sibling copies.
type failSource struct{}

func (failSource) Resolve(context.Context, name.Reference, name.Reference, []string) (*Plan, error) {
	return nil, nil
}

func (failSource) Fill(ctx context.Context, _, _ name.Repository, l PlannedLayer, _ ProgressSink) error {
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
	w, js := newCopier(t, nil, true)
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
	plan := &Plan{Layers: []PlannedLayer{{Digest: "sha256:1"}, {Digest: "sha256:2"}, {Digest: "sha256:3"}}}

	// The failing layer aborts its siblings while the dispatcher is still
	// admitting layers; the returned error must be the layer error either way.
	err := w.copyLayers(job.ctx, job, tr, failSource{}, plan, src_ref.Context(), dst_ref.Context())
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the layer error", err)
	}
	// The abort is scoped to the fan-out: the JOB's context is untouched, so a
	// later attempt or step of the same job still has a live context to run on.
	// Cancelling the job here would make every retry inherit a dead context —
	// which reads as "the job is being cancelled" and suppresses the retry.
	if cerr := job.ctx.Err(); cerr != nil {
		t.Errorf("job context = %v, want live after a layer failure", cerr)
	}
	if job.Canceled() {
		t.Error("a layer failure is not a cancellation of the job")
	}
	// It does stop attracting new callers, though: an identical submit must get a
	// fresh move rather than this failing one.
	if _, ok := js.Active(job.dedup); ok && job.dedup != "" {
		t.Error("a failing job should not be a coalescing target")
	}
	w.finish(job, err)
	snap, _ := js.Snapshot("job_l")
	if snap.State != JobFailed || snap.Err != "copy blob: boom" {
		t.Fatalf("state = %q err = %q, want failed with the layer error", snap.State, snap.Err)
	}
}

func TestFinish(t *testing.T) {
	t.Run("a real error beats a racing cancellation", func(t *testing.T) {
		w, js := newCopier(t, nil, true)
		ctx, cancel := context.WithCancel(context.Background())
		job := NewJob("job_f", "a/b:1", nil, time.Now())
		job.ctx, job.cancel = ctx, cancel
		if err := js.Add(job); err != nil {
			t.Fatal(err)
		}
		// A cancellation racing a real failure: the real error must win, so the
		// job is recorded as failed rather than silently canceled.
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
		w, js := newCopier(t, nil, true)
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
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background()
	req := Request{Ref: "team/app:1", Source: "docker.io", Target: "cache", Platforms: []string{"linux/amd64"}}
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
	w, js := newCopier(t, []config.StoreConfig{
		{Name: "cache", Kind: "oci", Host: "cache.local", Insecure: true, Mode: "copy"},
	}, true)
	w.base = context.Background()
	s1, _, err := w.Submit(Request{Ref: "team/app:1", Source: "docker.io", Target: "cache", Platforms: []string{"linux/amd64"}})
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

// The seal must land on every way out of copyLayers. The dispatch loop returns
// as soon as the abort reaches it, so a write placed after the fan-out would be
// skipped in exactly the case it exists for.
func TestLayerFailureSealsTheJobOnTheEarlyReturn(t *testing.T) {
	w, js := newCopier(t, nil, true)
	w.wc.MaxConcurrentLayers = 1 // force the dispatcher to block on the semaphore
	ctx, cancel := context.WithCancel(context.Background())
	job := NewJob("job_s", "a/b:1", nil, time.Now())
	job.ctx, job.cancel = ctx, cancel
	job.dedup = "k"
	tr := &Transfer{Layers: []*LayerProgress{{}, {}, {}}}
	job.Transfers = []*Transfer{tr}
	if err := js.Add(job); err != nil {
		t.Fatal(err)
	}
	src_ref, _ := name.ParseReference("up.local/a/b:1", name.Insecure)
	dst_ref, _ := name.ParseReference("cache.local/a/b:1", name.Insecure)
	plan := &Plan{Layers: []PlannedLayer{{Digest: "sha256:1"}, {Digest: "sha256:2"}, {Digest: "sha256:3"}}}

	if _, ok := js.Active("k"); !ok {
		t.Fatal("the job should start out coalesceable")
	}
	if err := w.copyLayers(job.ctx, job, tr, failSource{}, plan, src_ref.Context(), dst_ref.Context()); err == nil {
		t.Fatal("expected the layer error")
	}
	if _, ok := js.Active("k"); ok {
		t.Error("a failing job must stop being a coalescing target on every exit path")
	}
}

// flakySource breaks one blob part-way through its body, the way a reset
// connection does: some bytes are reported, then the copy fails. It succeeds
// once it has failed failFor times.
type flakySource struct {
	mu       sync.Mutex
	attempts map[string]int
	failFor  int
	partial  int64 // reported before breaking
	total    int64 // reported by an attempt that completes
	err      error // what a broken attempt returns (nil = a stream reset)
}

func (s *flakySource) Resolve(context.Context, name.Reference, name.Reference, []string) (*Plan, error) {
	return nil, nil
}

func (s *flakySource) Commit(context.Context, name.Reference, name.Reference, []string, bool) (v1.Hash, error) {
	return v1.Hash{}, nil
}

func (s *flakySource) count(digest string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[digest]
}

func (s *flakySource) Fill(_ context.Context, _, _ name.Repository, l PlannedLayer, sink ProgressSink) error {
	s.mu.Lock()
	if s.attempts == nil {
		s.attempts = map[string]int{}
	}
	s.attempts[l.Digest]++
	n := s.attempts[l.Digest]
	s.mu.Unlock()

	if n <= s.failFor {
		sink.Add(s.partial) // the body that did arrive before the break
		if s.err != nil {
			return s.err
		}
		return errors.New("push blob: stream error: stream ID 1135; INTERNAL_ERROR; received from peer")
	}
	sink.Add(s.total)
	sink.SetState("copied")
	return nil
}

// A blob that breaks part-way is attempted again, and the partial body of the
// broken attempt does not count toward the transfer: it never arrived, and the
// next attempt sends it again. Progress that only counted up would report the
// layer as more than complete and the transfer as having moved bytes it did not.
func TestABrokenBlobIsAttemptedAgain(t *testing.T) {
	w, js := newCopier(t, nil, true)
	w.wc.LayerAttempts = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := NewJob("job_r", "a/b:1", nil, time.Now())
	job.ctx, job.cancel = ctx, cancel
	lp := &LayerProgress{Total: 100}
	tr := &Transfer{BytesTotal: 100, Layers: []*LayerProgress{lp}}
	job.Transfers = []*Transfer{tr}
	if err := js.Add(job); err != nil {
		t.Fatal(err)
	}
	src_ref, _ := name.ParseReference("up.local/a/b:1", name.Insecure)
	dst_ref, _ := name.ParseReference("cache.local/a/b:1", name.Insecure)
	plan := &Plan{Layers: []PlannedLayer{{Digest: "sha256:1", Size: 100}}}
	src := &flakySource{failFor: 1, partial: 40, total: 100}

	if err := w.copyLayers(job.ctx, job, tr, src, plan, src_ref.Context(), dst_ref.Context()); err != nil {
		t.Fatalf("a blob that succeeds on its second attempt must not fail the transfer: %v", err)
	}
	if got := src.count("sha256:1"); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
	if got := lp.Done.Load(); got != 100 {
		t.Errorf("layer done = %d, want 100 (the 40 that never arrived must not count)", got)
	}
	if got := tr.BytesDone.Load(); got != 100 {
		t.Errorf("transfer done = %d, want 100", got)
	}
}

// A registry that ANSWERED has said what it holds, and asking again gets the
// same answer. Re-attempting it would only delay the fallback to another source
// by the whole backoff.
func TestADefiniteAnswerIsNotAttemptedAgain(t *testing.T) {
	w, js := newCopier(t, nil, true)
	w.wc.LayerAttempts = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := NewJob("job_a", "a/b:1", nil, time.Now())
	job.ctx, job.cancel = ctx, cancel
	tr := &Transfer{Layers: []*LayerProgress{{}}}
	job.Transfers = []*Transfer{tr}
	if err := js.Add(job); err != nil {
		t.Fatal(err)
	}
	src_ref, _ := name.ParseReference("up.local/a/b:1", name.Insecure)
	dst_ref, _ := name.ParseReference("cache.local/a/b:1", name.Insecure)
	plan := &Plan{Layers: []PlannedLayer{{Digest: "sha256:1"}}}
	src := &flakySource{failFor: 99, err: &transport.Error{StatusCode: http.StatusNotFound}}

	if err := w.copyLayers(job.ctx, job, tr, src, plan, src_ref.Context(), dst_ref.Context()); err == nil {
		t.Fatal("a 404 must still fail the transfer")
	}
	if got := src.count("sha256:1"); got != 1 {
		t.Errorf("attempts = %d, want 1: a definite answer is not re-attempted", got)
	}
}

func TestWorthAnotherAttempt(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tt := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"a reset connection", context.Background(), errors.New("stream error: INTERNAL_ERROR"), true},
		{"a 500", context.Background(), &transport.Error{StatusCode: 500}, true},
		{"a rate limit", context.Background(), &transport.Error{StatusCode: http.StatusTooManyRequests}, true},
		{"not found", context.Background(), &transport.Error{StatusCode: http.StatusNotFound}, false},
		{"unauthorized", context.Background(), &transport.Error{StatusCode: http.StatusUnauthorized}, false},
		{"forbidden", context.Background(), &transport.Error{StatusCode: http.StatusForbidden}, false},
		{"a cancelled job", dead, errors.New("stream error"), false},
		{"the cancellation itself", context.Background(), context.Canceled, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := worthAnotherAttempt(tt.ctx, tt.err); got != tt.want {
				t.Errorf("worthAnotherAttempt = %v, want %v", got, tt.want)
			}
		})
	}
}
