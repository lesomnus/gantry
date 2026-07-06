package warm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/z"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrQueueFull is returned by Submit when the pending-job buffer is saturated.
var ErrQueueFull = errors.New("warm queue is full")

// ErrJobNotFound is returned by Retry for an unknown job id.
var ErrJobNotFound = errors.New("job not found")

// ErrJobActive is returned by Retry when the job has not reached a terminal state.
var ErrJobActive = errors.New("job is still active")

// metrics are the otel instruments for the job pipeline (no-op when unconfigured).
type metrics struct {
	bytes    metric.Int64Counter
	duration metric.Float64Histogram
	active   metric.Int64UpDownCounter
	gauges   metric.Registration // queue depth/capacity + jobs-by-state observer
}

func newMetrics(ctx context.Context) *metrics {
	m := otx.Meter(ctx)
	bytes, _ := m.Int64Counter("gantry.bytes", metric.WithUnit("By"),
		metric.WithDescription("bytes moved between stores"))
	duration, _ := m.Float64Histogram("gantry.job.duration", metric.WithUnit("s"),
		metric.WithDescription("job duration"))
	active, _ := m.Int64UpDownCounter("gantry.jobs.active",
		metric.WithDescription("jobs in flight"))
	return &metrics{bytes: bytes, duration: duration, active: active}
}

// Request is a job submission: copy Ref from store From into store To. From and
// To are registry stores (a declared name or a bare host). Distributing the
// copied image onto an engine is a separate, explicit action (POST
// /v1/store/{name}/pull), not part of a job.
type Request struct {
	Ref       string
	Platforms []string
	From      string
	To        string
	// CopyReferrers copies the source's referrer artifacts (e.g. notation
	// signatures) into the cache alongside the image, preserving the source
	// digest so the signatures still verify there. nil defaults to on when
	// verification is enabled and the destination is a copy-mode cache.
	CopyReferrers *bool
}

// jobExec is the resolved plan for a job, computed at submit time.
type jobExec struct {
	from          config.StoreConfig
	to            config.StoreConfig
	hasCache      bool
	copyReferrers bool
	src           name.Reference
	dst           name.Reference        // valid only when hasCache
	cacheIdx      int                   // index of the registry transfer, or -1
	verification  *VerificationSnapshot // admission-time verification, stamped onto the Job

	// committed is the digest the cache copy landed at, set by runCopy.
	committed v1.Hash
}

// Warmer runs a two-tier worker pool: MaxConcurrentJobs jobs at once, each moving
// up to MaxConcurrentLayers layers at once during its registry copy.
type Warmer struct {
	stores *store.Set
	store  Store
	wc     config.WorkerConfig

	jobs     chan *Job
	idgen    func() string
	srcOpts  []name.Option // parse options for the source ref (tests inject name.Insecure)
	metrics  *metrics
	verifier verify.Verifier // source-signature verification (nil = disabled)
	rec      Recorder        // audit log (nil = disabled)

	base context.Context
	stop chan struct{} // closed by Stop; ends goroutines not fed by the jobs channel
	wg   sync.WaitGroup
}

func NewWarmer(stores *store.Set, jobStore Store, wc config.WorkerConfig) *Warmer {
	q := wc.QueueSize
	if q < 1 {
		q = 1
	}
	return &Warmer{
		stores:  stores,
		store:   jobStore,
		wc:      wc,
		jobs:    make(chan *Job, q),
		idgen:   newID,
		metrics: newMetrics(context.Background()),
		stop:    make(chan struct{}),
	}
}

// SetVerifier enables source-signature verification at job admission. Must be
// set before Start/Submit.
func (w *Warmer) SetVerifier(v verify.Verifier) { w.verifier = v }

// Recorder receives audit events for admitted and finished jobs. Its methods
// must not fail the operation they record.
type Recorder interface {
	JobAdmitted(ref, from, to, digest string)
	JobFinished(id, ref, state, errMsg string, bytes int64)
}

// SetRecorder wires the audit log. Must be set before Start/Submit.
func (w *Warmer) SetRecorder(r Recorder) { w.rec = r }

// rootCtx is the base context for admission-time work (verification); it uses
// the running server context once Start has been called.
func (w *Warmer) rootCtx() context.Context {
	if w.base != nil {
		return w.base
	}
	return context.Background()
}

// SetBaseContext sets the context new jobs derive from. Start calls it before
// spawning workers; a caller that only submits or plans (tests, or an
// admission-only warmer) can call it directly so submitted jobs enqueue but are
// never processed. Stop is safe without Start.
func (w *Warmer) SetBaseContext(ctx context.Context) { w.base = ctx }

func (w *Warmer) Start(ctx context.Context) {
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
func (w *Warmer) registerGauges(ctx context.Context) metric.Registration {
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

func (w *Warmer) Stop() {
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
func (w *Warmer) sweeper(ttl time.Duration) {
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
func (w *Warmer) Submit(req Request) (snap JobSnapshot, created bool, err error) {
	ex, transfers, platforms, err := w.plan(w.rootCtx(), req)
	if err != nil {
		return JobSnapshot{}, false, err
	}

	key := dedupKey(req.Ref, platforms, ex.from.Name, ex.to.Name)
	if snap, ok := w.store.Active(key); ok {
		return snap, false, nil
	}

	id := w.idgen()
	ctx, cancel := context.WithCancel(w.base)
	job := NewJob(id, req.Ref, platforms, time.Now())
	job.ctx, job.cancel, job.dedup, job.exec, job.req = ctx, cancel, key, ex, req
	job.Verification = ex.verification
	job.Transfers = transfers
	if err := w.store.Add(job); err != nil {
		cancel()
		return JobSnapshot{}, false, err
	}

	select {
	case w.jobs <- job:
		snap, _ := w.store.Snapshot(id)
		if w.rec != nil {
			digest := ""
			if ex.verification != nil {
				digest = ex.verification.Digest
			}
			w.rec.JobAdmitted(req.Ref, ex.from.Name, transferFrom(ex), digest)
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
func (w *Warmer) Retry(id string) (JobSnapshot, bool, error) {
	var req Request
	var state JobState
	// Read state and the original request together under the store lock.
	if ok := w.store.Update(id, func(j *Job) {
		state = j.State
		req = j.req
	}); !ok {
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
	From          string                `json:"from"`
	To            string                `json:"to,omitempty"`
	SrcRef        string                `json:"src_ref"`           // source ref, digest-pinned when verified
	DstRef        string                `json:"dst_ref,omitempty"` // rewritten cache ref
	Platforms     []string              `json:"platforms"`         // empty = all platforms
	CopyReferrers bool                  `json:"copy_referrers"`
	Verification  *VerificationSnapshot `json:"verification,omitempty"`
	Coalesces     string                `json:"coalesces,omitempty"` // active job an identical submit would join
}

// Plan dry-runs admission under the caller's context.
func (w *Warmer) Plan(ctx context.Context, req Request) (PlanResult, error) {
	ex, _, platforms, err := w.plan(ctx, req)
	if err != nil {
		return PlanResult{}, err
	}
	out := PlanResult{
		Ref:           req.Ref,
		From:          ex.from.Name,
		SrcRef:        ex.src.Name(),
		Platforms:     platforms,
		CopyReferrers: ex.copyReferrers,
		Verification:  ex.verification,
	}
	if ex.hasCache {
		out.To = ex.to.Name
		out.DstRef = ex.dst.Name()
	}
	key := dedupKey(req.Ref, platforms, ex.from.Name, ex.to.Name)
	if snap, ok := w.store.Active(key); ok {
		out.Coalesces = snap.ID
	}
	return out, nil
}

// plan resolves the request into an execution plan and the initial transfer rows.
func (w *Warmer) plan(ctx context.Context, req Request) (*jobExec, []*Transfer, []string, error) {
	platforms := w.platforms(req.Platforms)

	base, err := name.ParseReference(req.Ref, w.srcOpts...)
	if err != nil {
		return nil, nil, nil, z.Err(err, "parse ref %q", req.Ref)
	}
	repo, id := base.Context().RepositoryStr(), identifier(base)

	ex := &jobExec{cacheIdx: -1}

	// source (from)
	fromKey := req.From
	if fromKey == "" {
		fromKey = base.Context().RegistryStr()
	}
	ex.from, err = w.stores.Registry(fromKey)
	if err != nil {
		return nil, nil, nil, z.Err(err, "from")
	}
	ex.src, err = name.ParseReference(ex.from.Host+"/"+repo+id, w.refOpts(ex.from)...)
	if err != nil {
		return nil, nil, nil, z.Err(err, "source ref")
	}

	var transfers []*Transfer

	// cache fill (to)
	if req.To != "" {
		ex.to, err = w.stores.Registry(req.To)
		if err != nil {
			return nil, nil, nil, z.Err(err, "to")
		}
		ex.dst, err = Rewrite(ex.to.Rewrite, ex.to.Host, ex.src, ex.to.Insecure)
		if err != nil {
			return nil, nil, nil, z.Err(err, "rewrite into %q", ex.to.Name)
		}
		ex.hasCache = true
		ex.cacheIdx = len(transfers)
		transfers = append(transfers, &Transfer{
			Store: ex.to.Name, Kind: "oci", From: ex.from.Name,
			Ref: ex.dst.Name(), State: "pending",
		})
	}

	// Verify the source signature (fail-closed) before admitting the job, and pin
	// ex.src to the verified digest so the copy moves exactly what was
	// verified. Runs after the cache (dst) ref is derived from the tag, so the
	// cache stays tag-named; only the source pull is pinned to the digest.
	verified := false
	if w.verifier != nil {
		res, err := w.verifier.Verify(ctx, ex.from, ex.src)
		if err != nil {
			return nil, nil, nil, err // sentinel (ErrUnsigned/ErrUntrusted) preserved for the handler
		}
		ex.verification = &VerificationSnapshot{Mode: string(res.Mode), Verified: res.Verified()}
		dg := res.Digest
		if dg.Hex != "" {
			verified = true
			ex.verification.Digest = dg.String()
			// A proxy-mode cache reads through by tag and ignores the pinned source
			// digest, so it could fill from a different (unverified) image if the tag
			// moves after verification. Refuse rather than move unverified bytes.
			if ex.hasCache && ex.to.Mode == "proxy" {
				return nil, nil, nil, fmt.Errorf("signature verification requires a copy-mode destination; store %q is proxy", ex.to.Name)
			}
			ex.src = ex.src.Context().Digest(dg.String())
			log.From(w.rootCtx()).Info("source signature verified",
				slog.String("ref", req.Ref), slog.String("from", ex.from.Name), slog.String("digest", dg.String()))
		}
	}

	// Referrer propagation (signatures travel with the image): copy the source's
	// referrer artifacts into the cache with the source digest preserved — which
	// requires copying the image verbatim, i.e. every platform.
	if req.CopyReferrers != nil && *req.CopyReferrers && !ex.hasCache {
		return nil, nil, nil, fmt.Errorf("copy_referrers requires a copy-mode destination; set `to`")
	}
	if ex.hasCache {
		if req.CopyReferrers != nil {
			ex.copyReferrers = *req.CopyReferrers
		} else {
			// Default on only when this job actually verified a signature and
			// neither the request nor the config narrowed the platform set: the
			// pinned digest still protects a narrowed copy, only signature
			// propagation is skipped.
			ex.copyReferrers = verified && ex.to.Mode != "proxy" &&
				len(req.Platforms) == 0 && len(w.wc.Platforms) == 0
		}
		if ex.copyReferrers {
			if ex.to.Mode == "proxy" {
				return nil, nil, nil, fmt.Errorf("copy_referrers requires a copy-mode destination; store %q is proxy", ex.to.Name)
			}
			if len(req.Platforms) > 0 {
				return nil, nil, nil, fmt.Errorf("copy_referrers preserves the source image verbatim (all platforms); omit platforms or set copy_referrers to false")
			}
			if len(w.wc.Platforms) > 0 {
				// An explicit opt-in wins over the config narrowing, but widening
				// the copy to every platform must be observable.
				log.From(w.rootCtx()).Warn("copy_referrers overrides the configured platform narrowing; copying all platforms",
					slog.String("ref", req.Ref), slog.String("configured", strings.Join(w.wc.Platforms, ",")))
			}
			platforms = nil // the verbatim commit needs every child manifest present
		}
	}

	if len(transfers) == 0 {
		return nil, nil, nil, fmt.Errorf("job has nothing to do: set `to`")
	}
	return ex, transfers, platforms, nil
}

func transferFrom(ex *jobExec) string {
	if ex.hasCache {
		return ex.to.Name
	}
	return ex.from.Name
}

func (w *Warmer) worker() {
	defer w.wg.Done()
	for job := range w.jobs {
		w.run(job)
	}
}

func (w *Warmer) run(job *Job) {
	// Release the job's cancelCtx from the base context once it is terminal;
	// otherwise every job leaks a child context until its record is deleted.
	defer job.Cancel()
	ctx, span := otx.TraceStart(job.ctx, "job", trace.WithAttributes(attribute.String("gantry.ref", job.Ref)))
	defer span.End()
	l := log.From(ctx)
	l.Info("job started", slog.String("job", job.ID), slog.String("ref", job.Ref), slog.Int("transfers", len(job.Transfers)))
	w.metrics.active.Add(ctx, 1)
	defer w.metrics.active.Add(ctx, -1)

	start := time.Now()
	err := w.execute(ctx, job)
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

func (w *Warmer) execute(ctx context.Context, job *Job) error {
	w.store.Update(job.ID, func(j *Job) {
		j.State = JobRunning
		j.StartedAt = time.Now()
	})
	ex := job.exec

	if ex.hasCache {
		if err := w.runCopy(ctx, job, ex); err != nil {
			return err
		}
	}
	return nil
}

// runCopy fills the cache store (the registry transfer). Its failure fails the job.
func (w *Warmer) runCopy(ctx context.Context, job *Job, ex *jobExec) error {
	t := job.Transfers[ex.cacheIdx]
	w.store.Update(job.ID, func(*Job) { t.State = "running" })

	src, err := NewSource(ex.from, ex.to)
	if err != nil {
		return w.failTransfer(job, t, err)
	}
	plan, err := src.Resolve(ctx, ex.src, ex.dst, job.Platforms)
	if err != nil {
		return w.failTransfer(job, t, err)
	}
	w.store.Update(job.ID, func(*Job) {
		t.BytesTotal = plan.Total
		for _, pl := range plan.Layers {
			t.Layers = append(t.Layers, &LayerProgress{Digest: pl.Digest, Platform: pl.Platform, Total: pl.Size, State: "pending"})
		}
	})
	if err := w.copyLayers(ctx, job, t, src, plan, ex); err != nil {
		return w.failTransfer(job, t, err)
	}
	committed, err := src.Commit(ctx, ex.src, ex.dst, job.Platforms, ex.copyReferrers)
	if err != nil {
		return w.failTransfer(job, t, z.Err(err, "commit"))
	}
	ex.committed = committed
	if ex.copyReferrers {
		n, err := copyReferrers(ctx, ex.from, ex.to, ex.src, committed, ex.dst.Context())
		if err != nil {
			// Fail closed: the signatures are the point of referrer propagation.
			return w.failTransfer(job, t, z.Err(err, "copy referrers"))
		}
		log.From(ctx).Info("referrers copied",
			slog.Int("count", n), slog.String("subject", committed.String()), slog.String("store", ex.to.Name))
	}
	w.store.Update(job.ID, func(*Job) {
		if committed != (v1.Hash{}) {
			t.Digest = committed.String()
		}
		t.State = "done"
	})
	log.From(ctx).Info("cache filled",
		slog.String("store", ex.to.Name), slog.String("ref", ex.dst.Name()), slog.Int64("bytes", t.BytesDone.Load()))
	return nil
}

func (w *Warmer) copyLayers(ctx context.Context, job *Job, t *Transfer, src Source, plan *Plan, ex *jobExec) error {
	c := w.wc.MaxConcurrentLayers
	if c < 1 {
		c = 1
	}
	sem := make(chan struct{}, c)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	from := ex.src.Context()
	to := ex.dst.Context()
	for i := range plan.Layers {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			// A failing layer cancels ctx to abort the dispatch, so the real error,
			// not the derived cancellation, is the job's outcome. (wg.Wait orders
			// the once.Do write of firstErr before this read.)
			if firstErr != nil && !errors.Is(firstErr, context.Canceled) {
				return firstErr
			}
			return ctx.Err()
		}
		wg.Add(1)
		go func(pl PlannedLayer, lp *LayerProgress) {
			defer wg.Done()
			defer func() { <-sem }()
			w.store.Update(job.ID, func(*Job) { lp.State = "pulling" })
			sink := &layerSink{w: w, jobID: job.ID, t: t, lp: lp}
			if err := src.Warm(ctx, from, to, pl, sink); err != nil {
				w.store.Update(job.ID, func(*Job) { lp.State = "failed" })
				once.Do(func() {
					firstErr = err
					job.Cancel()
				})
			}
		}(plan.Layers[i], t.Layers[i])
	}
	wg.Wait()
	return firstErr
}

func (w *Warmer) failTransfer(job *Job, t *Transfer, err error) error {
	w.store.Update(job.ID, func(*Job) {
		t.State = "failed"
		t.Err = err.Error()
	})
	return err
}

func (w *Warmer) finish(job *Job, err error) {
	w.store.Update(job.ID, func(j *Job) {
		if j.State.Terminal() {
			return
		}
		switch {
		case err == nil:
			j.State = JobDone
		case errors.Is(err, context.Canceled):
			// Only an error that IS the cancellation counts as canceled. A layer
			// failure cancels job.ctx to abort its sibling copies, but the job must
			// be recorded as failed with the real error, not silently "canceled".
			j.State = JobCanceled
		default:
			j.State = JobFailed
			j.Err = err.Error()
		}
		j.EndedAt = time.Now()
	})
}

func (w *Warmer) platforms(req []string) []string {
	if len(req) > 0 {
		return req
	}
	if len(w.wc.Platforms) > 0 {
		return w.wc.Platforms
	}
	return []string{runtime.GOOS + "/" + runtime.GOARCH}
}

// refOpts returns name parse options for a registry store (insecure + test opts).
func (w *Warmer) refOpts(c config.StoreConfig) []name.Option {
	opts := append([]name.Option(nil), w.srcOpts...)
	if c.Insecure {
		opts = append(opts, name.Insecure)
	}
	return opts
}

// layerSink reports a registry-copy blob's progress.
type layerSink struct {
	w     *Warmer
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
		panic(fmt.Sprintf("warm: read random: %v", err))
	}
	return "job_" + hex.EncodeToString(b[:])
}
