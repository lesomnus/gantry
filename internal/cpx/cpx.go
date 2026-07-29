package cpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrQueueFull is returned by Submit when the pending-job buffer is saturated.
var ErrQueueFull = errors.New("job queue is full")

// ErrJobNotFound is returned by Retry for an unknown job id.
var ErrJobNotFound = errors.New("job not found")

// ErrJobActive is returned by Retry when the job has not reached a terminal state.
var ErrJobActive = errors.New("job is still active")

// metrics are the otel instruments for the job pipeline (no-op when unconfigured).
type metrics struct {
	bytes    metric.Int64Counter
	duration metric.Float64Histogram
	active   metric.Int64UpDownCounter
	fallback metric.Int64Counter     // engine pulls re-attempted against another source
	srcWait  metric.Float64Histogram // time spent waiting for an in-flight fill
	gauges   metric.Registration     // queue depth/capacity + jobs-by-state observer
}

func newMetrics(ctx context.Context) *metrics {
	m := otx.Meter(ctx)
	bytes, _ := m.Int64Counter("gantry.bytes", metric.WithUnit("By"),
		metric.WithDescription("bytes moved between stores"))
	duration, _ := m.Float64Histogram("gantry.job.duration", metric.WithUnit("s"),
		metric.WithDescription("job duration"))
	active, _ := m.Int64UpDownCounter("gantry.jobs.active",
		metric.WithDescription("jobs in flight"))
	// A cache that quietly stops being used looks like success everywhere else:
	// the job is done, the node has the image. This counter is the signal that
	// it was served from somewhere other than where the operator intended.
	fallback, _ := m.Int64Counter("gantry.job.fallback",
		metric.WithDescription("engine pulls retried against a fallback source"))
	// Whether waiting for a fill is worth its bound is an operational question,
	// not a design one: the outcome label answers it per deployment.
	srcWait, _ := m.Float64Histogram("gantry.job.source_wait", metric.WithUnit("s"),
		metric.WithDescription("time an engine pull spent waiting for an in-flight fill of its source"))
	return &metrics{bytes: bytes, duration: duration, active: active, fallback: fallback, srcWait: srcWait}
}

// Request is a job submission: move Ref from store Source into store Target.
// Source is a registry store (a declared name or a bare host). Target is any
// declared store — an oci registry (gantry copies the blobs in) or a
// docker/containerd engine (the daemon is told to pull). The user just "moves"
// the image; how it lands depends on what the target can do.
type Request struct {
	Ref string
	// Platforms selects what moves. Registry target: the platforms copied
	// (empty = every platform). Engine target: the single platform the
	// daemon pulls (empty = the daemon host's platform; more than one errors).
	Platforms []string
	Source    string
	Target    string
	// CopyReferrers copies the source's referrer artifacts (e.g. notation
	// signatures) into the cache alongside the image, preserving the source
	// digest so the signatures still verify there. nil defaults to on when
	// verification is enabled and the target is a copy-mode cache.
	// Registry targets only.
	CopyReferrers *bool
	// As records the pulled image under these names instead of the pull
	// reference, so a cache-fed engine can keep the image under its upstream
	// name. Tag references — or, for a digest-pinned job (a digest ref, or a
	// verified source), digest references carrying the pinned digest: those are
	// registered over the pulled content so the upstream digest name resolves
	// locally without touching its registry (containerd image store only; a
	// classic graph-driver docker rejects the job before pulling). Engine
	// targets only.
	As []string
	// FallbackToOrigin lets an engine pull that its source could not serve
	// retry against the registry named in Ref (the origin), so a cache is an
	// optimization rather than a dependency: whether the cache is empty because
	// its fill job failed, has not run yet, or is unreachable is one
	// indistinguishable fact from the pull's side, and none of them need fail
	// the job. nil takes the server default (worker.fallback_to_origin).
	// Engine targets only.
	FallbackToOrigin *bool
	// Labels is caller metadata attached to the job for List filtering; it does
	// not affect the move or coalescing. Each coalesced caller keeps its own.
	Labels map[string]string
}

// Copier runs a two-tier worker pool: MaxConcurrentJobs jobs at once, each moving
// up to MaxConcurrentLayers layers at once during its registry copy.
type Copier struct {
	stores *store.Set
	store  Store
	wc     config.WorkerConfig

	jobs     chan *Job
	idgen    func() string
	srcOpts  []name.Option // parse options for the source ref (tests inject name.Insecure)
	metrics  *metrics
	pullHook func(engine, ref string) // notified after a successful engine-destination pull (retention)
	verifier verify.Verifier          // source-signature verification (nil = disabled)
	rec      Recorder                 // audit log (nil = disabled)

	// waitSlots bounds how many running jobs may be parked waiting for another
	// job to fill their source. Sized below the worker count so a worker is
	// always left to run the fills themselves.
	waitSlots chan struct{}

	base context.Context
	stop chan struct{} // closed by Stop; ends goroutines not fed by the jobs channel
	wg   sync.WaitGroup
}

func NewCopier(stores *store.Set, jobStore Store, wc config.WorkerConfig) *Copier {
	q := wc.QueueSize
	if q < 1 {
		q = 1
	}
	// One fewer than the worker pool: a worker parked on a fill is a worker not
	// running one, so the last slot is always reserved for making progress.
	waits := wc.MaxConcurrentJobs - 1
	if waits < 1 {
		waits = 1
	}
	return &Copier{
		stores:    stores,
		store:     jobStore,
		wc:        wc,
		jobs:      make(chan *Job, q),
		idgen:     newID,
		metrics:   newMetrics(context.Background()),
		waitSlots: make(chan struct{}, waits),
		stop:      make(chan struct{}),
	}
}

// SetPullHook registers a callback invoked (engine name, ref) after a job's
// engine destination completes its pull — used to stamp the retention index.
func (w *Copier) SetPullHook(fn func(engine, ref string)) { w.pullHook = fn }

// SetVerifier enables source-signature verification at job admission. Must be
// set before Start/Submit.
func (w *Copier) SetVerifier(v verify.Verifier) { w.verifier = v }

// Recorder receives audit events for admitted and finished jobs. Its methods
// must not fail the operation they record.
type Recorder interface {
	JobAdmitted(id, ref, source, target, digest string)
	JobFinished(id, ref, state, errMsg string, bytes int64)
	// JobFellBack records that `from` could not serve the job and `to` was tried
	// instead. Called when the second attempt starts, so it lands even if that
	// one fails too. Durable, unlike the job record's failed transfer row.
	JobFellBack(id, ref, from, to, cause string)
}

// SetRecorder wires the audit log. Must be set before Start/Submit.
func (w *Copier) SetRecorder(r Recorder) { w.rec = r }

// rootCtx is the base context for admission-time work (verification); it uses
// the running server context once Start has been called.
func (w *Copier) rootCtx() context.Context {
	if w.base != nil {
		return w.base
	}
	return context.Background()
}

// SetBaseContext sets the context new jobs derive from. Start calls it before
// spawning workers; a caller that only submits or plans (tests, or an
// admission-only copier) can call it directly so submitted jobs enqueue but are
// never processed. Stop is safe without Start.
func (w *Copier) SetBaseContext(ctx context.Context) { w.base = ctx }

func (w *Copier) Start(ctx context.Context) {
	w.base = ctx
	w.metrics = newMetrics(ctx)
	w.metrics.gauges = w.registerGauges(ctx)
	n := w.wc.MaxConcurrentJobs
	if n < 1 {
		n = 1
	}
	for range n {
		w.wg.Add(1)
		go w.worker()
	}
	if ttl := time.Duration(w.wc.JobTTL); ttl > 0 {
		w.wg.Add(1)
		go w.sweeper(ttl)
	}
}

// registerGauges observes queue saturation (visible before clients hit the 503
// of a full queue) and job records by state.
func (w *Copier) registerGauges(ctx context.Context) metric.Registration {
	m := otx.Meter(ctx)
	depth, _ := m.Int64ObservableGauge("gantry.queue.depth",
		metric.WithDescription("jobs waiting in the pending-job queue"))
	capacity, _ := m.Int64ObservableGauge("gantry.queue.capacity",
		metric.WithDescription("pending-job queue buffer size"))
	jobs, _ := m.Int64ObservableGauge("gantry.jobs",
		metric.WithDescription("job records by state"))
	reg, _ := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(depth, int64(len(w.jobs)))
		o.ObserveInt64(capacity, int64(cap(w.jobs)))
		counts := w.store.Counts()
		for _, st := range []JobState{JobPending, JobRunning, JobDone, JobFailed, JobCanceled} {
			o.ObserveInt64(jobs, int64(counts[st]), metric.WithAttributes(attribute.String("state", string(st))))
		}
		return nil
	}, depth, capacity, jobs)
	return reg
}

func (w *Copier) Stop() {
	close(w.stop)
	close(w.jobs)
	w.wg.Wait()
	xport.CloseTPM() // release any TPM devices opened for mTLS transports
	if w.metrics.gauges != nil {
		_ = w.metrics.gauges.Unregister()
	}
}

// sweeper evicts terminal job records older than JobTTL so they do not
// accumulate unbounded on a long-lived server.
func (w *Copier) sweeper(ttl time.Duration) {
	defer w.wg.Done()
	period := min(ttl/4, time.Minute)
	if period <= 0 {
		period = time.Millisecond
	}
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-w.base.Done():
			return
		case <-w.stop:
			return
		case <-tick.C:
			w.store.Sweep(time.Now(), ttl)
		}
	}
}

// Submit resolves and enqueues a job, collapsing identical in-flight moves.
// created reports whether a new job was enqueued (false = coalesced onto an
// active identical move).
func (w *Copier) Submit(req Request) (snap JobSnapshot, created bool, err error) {
	p, err := w.plan(w.rootCtx(), req)
	if err != nil {
		return JobSnapshot{}, false, err
	}

	key := dedupKey(req.Ref, p.platforms, p.source.Name, p.target.Name(), p.as, p.fallback)
	now := time.Now()
	id := w.idgen()
	// Coalesce onto an identical in-flight move, but hand this caller its own
	// handle so its labels and its cancel stay independent of the others'.
	if snap, ok := w.store.Attach(key, id, req.Labels, now); ok {
		return snap, false, nil
	}

	// id doubles as the new execution's id and its primary handle's id.
	ctx, cancel := context.WithCancel(w.base)
	job := NewJob(id, req.Ref, p.platforms, now)
	job.ctx, job.cancel, job.dedup, job.exec, job.req = ctx, cancel, key, p, req
	job.As = p.as
	job.FallbackToOrigin = p.fallback
	job.Fills = p.fills()
	job.Source, job.Target = p.source.Name, p.target.Name()
	job.Labels = req.Labels
	job.Verification = p.verification
	// One pending row per step: the route as intended, visible to a client
	// watching a job that has not started. Attempts claim and append to it.
	for _, st := range p.steps {
		job.Transfers = append(job.Transfers, st.seed())
	}
	// Publish but keep it un-coalescable until it is safely queued: a racing
	// identical submit must not attach to a job that then fails to enqueue and
	// is rolled back below, which would strand the coalescer on a job that
	// never runs.
	job.enqueuing = true
	if err := w.store.Add(job); err != nil {
		cancel()
		return JobSnapshot{}, false, err
	}

	select {
	case w.jobs <- job:
		w.store.Update(id, func(j *Job) { j.enqueuing = false })
		snap, _ := w.store.Snapshot(id)
		if w.rec != nil {
			digest := ""
			if p.verification != nil {
				digest = p.verification.Digest
			}
			w.rec.JobAdmitted(id, req.Ref, p.source.Name, p.target.Name(), digest)
		}
		return snap, true, nil
	default:
		w.store.Delete(id)
		return JobSnapshot{}, false, ErrQueueFull
	}
}

// Retry re-submits a terminal job's ORIGINAL request: fresh store resolution,
// fresh signature verification, fresh digest pin — never the stored plan, whose
// digest was pinned at the original admission.
func (w *Copier) Retry(id string) (JobSnapshot, bool, error) {
	// Read this handle's request and its per-caller state: a coalesced caller
	// retries with its own labels and its own (possibly canceled) view, not the
	// originating submit's — which is what every other RPC reports for this id.
	req, state, ok := w.store.RetrySource(id)
	if !ok {
		return JobSnapshot{}, false, ErrJobNotFound
	}
	if !state.Terminal() {
		return JobSnapshot{}, false, fmt.Errorf("%w: %s is %s", ErrJobActive, id, state)
	}
	return w.Submit(req)
}

// PlanResult is the fully resolved admission plan — store bindings, rewritten
// refs, and the verification outcome — without moving bytes or creating a job.
type PlanResult struct {
	Ref           string                `json:"ref"`
	Source        string                `json:"source"`
	Target        string                `json:"target,omitempty"`
	SourceRef     string                `json:"source_ref"`           // source ref, digest-pinned when verified
	TargetRef     string                `json:"target_ref,omitempty"` // target-side ref: the rewritten cache ref, or the ref the engine is told to pull
	Platforms     []string              `json:"platforms"`            // registry target: empty = all platforms; engine target: the single platform pulled
	As            []string              `json:"as,omitempty"`         // engine target: names the image is recorded under
	CopyReferrers bool                  `json:"copy_referrers"`
	Verification  *VerificationSnapshot `json:"verification,omitempty"`
	Coalesces     string                `json:"coalesces,omitempty"` // active job an identical submit would join
	// FallbackToOrigin is the effective value after the server default.
	FallbackToOrigin bool `json:"fallback_to_origin"`
	// FallbackRef is the ref the engine would be told to pull if its source
	// could not serve the image. Empty when the job has no fallback: the flag is
	// off, the source already is the origin, or the origin is not addressable
	// from this engine.
	FallbackRef string `json:"fallback_ref,omitempty"`
	// Steps is the route the job would run, in order — one entry per hop, each
	// listing the sources that hop would try. A one-entry list with one attempt is
	// the ordinary single-hop move. Advisory: coalescing is request-level, so a
	// submit can be served by an active job that resolved a different route.
	Steps []PlanStep `json:"steps"`
}

// PlanStep is one hop of a planned route.
type PlanStep struct {
	Store string `json:"store"` // the store this hop fills
	Kind  string `json:"kind" enums:"oci,docker,containerd"`
	Ref   string `json:"ref"` // the reference this hop lands in that store
	// Sources are the places this hop would read from, in the order they would be
	// tried.
	Sources []PlanSource `json:"sources"`
	// Optional marks a hop gantry added for its own benefit, whose failure the job
	// tolerates.
	Optional bool `json:"optional,omitempty"`
}

// PlanSource is one place a hop would read from.
type PlanSource struct {
	Store string `json:"store"`
	Ref   string `json:"ref"`
	// Why the source is in the list: `planned` (the one the caller named), `route`
	// (a nearer copy gantry chose), `origin` (the registry named in the job's ref).
	Why string `json:"why" enums:"planned,route,origin"`
}

// Plan dry-runs admission under the caller's context.
func (w *Copier) Plan(ctx context.Context, req Request) (PlanResult, error) {
	p, err := w.plan(ctx, req)
	if err != nil {
		return PlanResult{}, err
	}
	last := p.last()
	out := PlanResult{
		Ref:              req.Ref,
		Source:           p.source.Name,
		Target:           p.target.Name(),
		SourceRef:        p.authorityRef.Name(),
		TargetRef:        last.rowRef(last.attempts[0]),
		Platforms:        p.platforms,
		As:               p.as,
		CopyReferrers:    p.copyReferrers,
		Verification:     p.verification,
		FallbackToOrigin: p.fallback,
	}
	for _, st := range p.steps {
		ps := PlanStep{Store: st.dst.Name(), Kind: st.dst.Kind(), Optional: st.optional}
		if st.ref != nil {
			ps.Ref = st.ref.Name()
		}
		for _, at := range st.attempts {
			ps.Sources = append(ps.Sources, PlanSource{Store: at.src.Name, Ref: at.ref.Name(), Why: string(at.why)})
			if at.why == whyOrigin {
				out.FallbackRef = st.rowRef(at)
			}
		}
		out.Steps = append(out.Steps, ps)
	}
	key := dedupKey(req.Ref, p.platforms, p.source.Name, p.target.Name(), p.as, p.fallback)
	if snap, ok := w.store.Active(key); ok {
		out.Coalesces = snap.ID
	}
	return out, nil
}

func (w *Copier) worker() {
	defer w.wg.Done()
	for job := range w.jobs {
		w.run(job)
	}
}

func (w *Copier) run(job *Job) {
	// Release the job's cancelCtx from the base context once it is terminal;
	// otherwise every job leaks a child context until its record is deleted.
	defer job.Cancel()
	// …and release anyone waiting for this job's output. Deferred here rather
	// than in finish() so an erased or evicted record still frees its waiters.
	defer job.markDone()
	ctx, span := otx.TraceStart(job.ctx, "job", trace.WithAttributes(attribute.String("gantry.ref", job.Ref)))
	defer span.End()
	l := log.From(ctx)
	l.Info("job started", slog.String("job", job.ID), slog.String("ref", job.Ref), slog.Int("transfers", len(job.Transfers)))
	w.metrics.active.Add(ctx, 1)
	defer w.metrics.active.Add(ctx, -1)

	start := time.Now()
	err := w.execute(ctx, job)
	// An attempt aborts its own work on failure, and those aborts are scoped so
	// they cannot reach the job's context. Should one still surface as a
	// cancellation, break the errors.Is chain here, where the error leaves the
	// job: finish() maps a cancellation to JobCanceled, and a job nobody withdrew
	// must not be recorded as withdrawn. Classification inside the attempt loop
	// sees the error as it came, so "the source reported a cancellation" still
	// counts as a reason not to try elsewhere.
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() == nil {
		err = fmt.Errorf("aborted: %s", err)
	}
	w.finish(job, err)

	state := "done"
	switch {
	case err == nil:
		l.Info("job done", slog.String("job", job.ID), slog.String("ref", job.Ref),
			slog.Int64("bytes", jobBytes(job)), slog.Int64("took_ms", time.Since(start).Milliseconds()))
	case errors.Is(err, context.Canceled):
		// Cancellation (DELETE /v1/job or shutdown) is a normal terminal state
		// (finish() maps it to JobCanceled), not a failure — don't alert on it.
		// A real error wins even though a layer failure also cancels job.ctx to
		// abort its siblings.
		state = "canceled"
		l.Info("job canceled", slog.String("job", job.ID), slog.String("ref", job.Ref))
	default:
		state = "failed"
		span.RecordError(err)
		l.Error("job failed", slog.String("job", job.ID), slog.String("ref", job.Ref), slog.String("error", err.Error()))
	}
	w.metrics.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("state", state)))
	w.metrics.bytes.Add(ctx, jobBytes(job))
	if w.rec != nil {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		w.rec.JobFinished(job.ID, job.Ref, state, errMsg, jobBytes(job))
	}
}

// waitForFill blocks until an active job that is filling this job's source with
// exactly this image finishes, and reports whether it is worth re-attempting
// that source. It runs only after a real miss, so a source that can already
// serve the image is never delayed by it.
//
// The wait holds a worker, so it takes one of a bounded number of slots: with
// every worker parked on a fill, nothing would be left to run the fills
// themselves. A job that cannot take a slot goes straight on to the fallback.
func (w *Copier) waitForFill(ctx context.Context, job *Job, want string) bool {
	limit := time.Duration(w.wc.SourceWait)
	if limit <= 0 || want == "" {
		return false
	}
	done, ok := w.store.Filling(want)
	if !ok {
		return false
	}
	select {
	case w.waitSlots <- struct{}{}:
		defer func() { <-w.waitSlots }()
	default:
		log.From(ctx).Info("not waiting for an in-flight fill: no wait slot free",
			slog.String("job", job.ID), slog.String("ref", want))
		w.metrics.srcWait.Record(ctx, 0, metric.WithAttributes(attribute.String("outcome", "skipped")))
		return false
	}

	l := log.From(ctx)
	l.Info("waiting for an in-flight fill of this job's source",
		slog.String("job", job.ID), slog.String("ref", want), slog.Duration("limit", limit))
	start := time.Now()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	record := func(outcome string) {
		w.metrics.srcWait.Record(ctx, time.Since(start).Seconds(),
			metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	select {
	case <-done:
		// The fill finished — it may have failed, in which case the retry costs
		// one cheap miss and the fallback follows. Retrying is still right: only
		// the source itself can say whether it now holds the image.
		l.Info("in-flight fill finished; re-attempting the source",
			slog.String("job", job.ID), slog.Duration("waited", time.Since(start)))
		record("served")
		return true
	case <-timer.C:
		l.Warn("gave up waiting for an in-flight fill",
			slog.String("job", job.ID), slog.String("ref", want), slog.Duration("waited", time.Since(start)))
		record("timeout")
		return false
	case <-ctx.Done():
		record("canceled")
		return false
	}
}

// engineSink folds an engine's per-layer reports into a Transfer, upserting
// layers by digest and recomputing the transfer totals.
type engineSink struct {
	w     *Copier
	jobID string
	t     *Transfer
	idx   map[string]*LayerProgress
}

func (s *engineSink) Layer(u down.LayerUpdate) {
	s.w.store.Update(s.jobID, func(*Job) {
		lp := s.idx[u.Digest]
		if lp == nil {
			lp = &LayerProgress{Digest: u.Digest, State: "pulling"}
			s.idx[u.Digest] = lp
			s.t.Layers = append(s.t.Layers, lp)
		}
		if u.Total > 0 {
			lp.Total = u.Total
		}
		switch u.State {
		case "exists":
			lp.State = "exists"
			lp.Done.Store(lp.Total)
		case "done":
			lp.State = "done"
			if lp.Total > 0 {
				lp.Done.Store(lp.Total)
			}
		default:
			lp.State = "pulling"
			lp.Done.Store(u.Done)
		}
		var tot, done int64
		for _, l := range s.t.Layers {
			tot += l.Total
			done += l.Done.Load()
		}
		s.t.BytesTotal = tot
		s.t.BytesDone.Store(done)
	})
}

func (w *Copier) copyLayers(ctx context.Context, job *Job, t *Transfer, src Source, plan *Plan, srcRepo, dstRepo name.Repository) error {
	c := w.wc.MaxConcurrentLayers
	if c < 1 {
		c = 1
	}
	// A failing layer aborts its siblings, and nothing beyond them: the abort is
	// scoped to this fan-out, not to the job. Cancelling the JOB here would leave
	// every later attempt or step of the same job starting on a dead context — and
	// a dead context is read as "the job is being cancelled", which suppresses the
	// retry that was supposed to recover from this very failure.
	lctx, abort := context.WithCancel(ctx)
	defer abort()
	sem := make(chan struct{}, c)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for i := range plan.Layers {
		select {
		case sem <- struct{}{}:
		case <-lctx.Done():
			wg.Wait()
			// The abort above is derived from the real failure, so report that
			// rather than the cancellation it caused. (wg.Wait orders the once.Do
			// write of firstErr before this read.)
			if firstErr != nil && !errors.Is(firstErr, context.Canceled) {
				return firstErr
			}
			return lctx.Err()
		}
		wg.Add(1)
		go func(pl PlannedLayer, lp *LayerProgress) {
			defer wg.Done()
			defer func() { <-sem }()
			w.store.Update(job.ID, func(*Job) { lp.State = "pulling" })
			sink := &layerSink{w: w, jobID: job.ID, t: t, lp: lp}
			if err := src.Fill(lctx, srcRepo, dstRepo, pl, sink); err != nil {
				w.store.Update(job.ID, func(*Job) { lp.State = "failed" })
				once.Do(func() {
					firstErr = err
					abort()
					// Sealed here rather than on the way out: the dispatch loop
					// above returns early once the abort lands, so a bottom-of-
					// function write would be skipped exactly when it matters.
					w.store.Update(job.ID, func(j *Job) { j.sealed = true })
				})
			}
		}(plan.Layers[i], t.Layers[i])
	}
	wg.Wait()
	return firstErr
}

func (w *Copier) failTransfer(job *Job, t *Transfer, err error) error {
	w.store.Update(job.ID, func(*Job) {
		t.State = "failed"
		t.Err = err.Error()
	})
	return err
}

func (w *Copier) finish(job *Job, err error) {
	w.store.Update(job.ID, func(j *Job) {
		if j.State.Terminal() {
			return
		}
		switch {
		case err == nil:
			j.State = JobDone
		case errors.Is(err, context.Canceled):
			// Only an error that IS the cancellation counts as canceled. An abort
			// derived from a real failure (a layer aborting its siblings, an
			// attempt aborting) reports that failure instead, and is scoped so it
			// never reaches job.ctx in the first place.
			j.State = JobCanceled
		default:
			j.State = JobFailed
			j.Err = err.Error()
		}
		j.DateEnded = time.Now()
	})
}

// refOpts returns name parse options for a registry store (insecure + test opts).
func (w *Copier) refOpts(c config.StoreConfig) []name.Option {
	opts := append([]name.Option(nil), w.srcOpts...)
	if c.Insecure {
		opts = append(opts, name.Insecure)
	}
	return opts
}

// layerSink reports a registry-copy blob's progress.
type layerSink struct {
	w     *Copier
	jobID string
	t     *Transfer
	lp    *LayerProgress
}

func (s *layerSink) Add(n int64) {
	s.lp.Done.Add(n)
	s.t.BytesDone.Add(n)
}

func (s *layerSink) SetState(state string) {
	s.w.store.Update(s.jobID, func(*Job) { s.lp.State = state })
}

func identifier(ref name.Reference) string {
	if d, ok := ref.(name.Digest); ok {
		return "@" + d.DigestStr()
	}
	return ":" + ref.Identifier()
}

func jobBytes(job *Job) int64 {
	var n int64
	for _, t := range job.Transfers {
		n += t.BytesDone.Load()
	}
	return n
}

func newID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("cpx: read random: %v", err))
	}
	return "job_" + hex.EncodeToString(b[:])
}
