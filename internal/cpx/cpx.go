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
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
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
	// locally without touching its registry (containerd image store; a classic
	// docker graph store skips them with a warning). Engine targets only.
	As []string
	// Labels is caller metadata attached to the job for List filtering; it does
	// not affect the move or coalescing. Each coalesced caller keeps its own.
	Labels map[string]string
}

// jobExec is the resolved plan for a job, computed at submit time.
type jobExec struct {
	source        config.StoreConfig
	dst           dest // the target; pusher or puller by capability
	copyReferrers bool
	verbatim      bool                  // pusher dest: commit the source manifest/index byte-for-byte
	srcRef        name.Reference        // source ref; digest-pinned when verified
	cacheRef      name.Reference        // pusher dest: the rewritten in-store ref
	pullRef       string                // puller dest: the ref the engine is told to pull
	as            []string              // puller dest: names recorded instead of pullRef
	asDigest      bool                  // puller dest: as contains digest references (anchor bytes needed)
	platform      string                // puller dest: the resolved platform
	verification  *VerificationSnapshot // admission-time verification, stamped onto the Job

	// committed is the digest the cache copy landed at, set by runCopy.
	committed v1.Hash
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

	base context.Context
	stop chan struct{} // closed by Stop; ends goroutines not fed by the jobs channel
	wg   sync.WaitGroup
}

func NewCopier(stores *store.Set, jobStore Store, wc config.WorkerConfig) *Copier {
	q := wc.QueueSize
	if q < 1 {
		q = 1
	}
	return &Copier{
		stores:  stores,
		store:   jobStore,
		wc:      wc,
		jobs:    make(chan *Job, q),
		idgen:   newID,
		metrics: newMetrics(context.Background()),
		stop:    make(chan struct{}),
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
	ex, transfers, platforms, err := w.plan(w.rootCtx(), req)
	if err != nil {
		return JobSnapshot{}, false, err
	}

	key := dedupKey(req.Ref, platforms, ex.source.Name, ex.dst.Name(), ex.as)
	now := time.Now()
	id := w.idgen()
	// Coalesce onto an identical in-flight move, but hand this caller its own
	// handle so its labels and its cancel stay independent of the others'.
	if snap, ok := w.store.Attach(key, id, req.Labels, now); ok {
		return snap, false, nil
	}

	// id doubles as the new execution's id and its primary handle's id.
	ctx, cancel := context.WithCancel(w.base)
	job := NewJob(id, req.Ref, platforms, now)
	job.ctx, job.cancel, job.dedup, job.exec, job.req = ctx, cancel, key, ex, req
	job.As = ex.as
	job.Labels = req.Labels
	job.Verification = ex.verification
	job.Transfers = transfers
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
			if ex.verification != nil {
				digest = ex.verification.Digest
			}
			w.rec.JobAdmitted(id, req.Ref, ex.source.Name, ex.dst.Name(), digest)
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
}

// Plan dry-runs admission under the caller's context.
func (w *Copier) Plan(ctx context.Context, req Request) (PlanResult, error) {
	ex, _, platforms, err := w.plan(ctx, req)
	if err != nil {
		return PlanResult{}, err
	}
	out := PlanResult{
		Ref:           req.Ref,
		Source:        ex.source.Name,
		Target:        ex.dst.Name(),
		SourceRef:     ex.srcRef.Name(),
		Platforms:     platforms,
		As:            ex.as,
		CopyReferrers: ex.copyReferrers,
		Verification:  ex.verification,
	}
	switch {
	case ex.cacheRef != nil:
		out.TargetRef = ex.cacheRef.Name()
	case ex.pullRef != "":
		out.TargetRef = ex.pullRef
	}
	key := dedupKey(req.Ref, platforms, ex.source.Name, ex.dst.Name(), ex.as)
	if snap, ok := w.store.Active(key); ok {
		out.Coalesces = snap.ID
	}
	return out, nil
}

// plan resolves the request into an execution plan and the initial transfer row.
func (w *Copier) plan(ctx context.Context, req Request) (*jobExec, []*Transfer, []string, error) {
	base, err := name.ParseReference(req.Ref, w.srcOpts...)
	if err != nil {
		return nil, nil, nil, z.Err(err, "parse ref %q", req.Ref)
	}
	repo, id := base.Context().RepositoryStr(), identifier(base)

	ex := &jobExec{}

	// source
	sourceKey := req.Source
	if sourceKey == "" {
		sourceKey = base.Context().RegistryStr()
	}
	ex.source, err = w.stores.Registry(sourceKey)
	if err != nil {
		return nil, nil, nil, z.Err(err, "source")
	}
	ex.srcRef, err = name.ParseReference(ex.source.Host+"/"+repo+id, w.refOpts(ex.source)...)
	if err != nil {
		return nil, nil, nil, z.Err(err, "source ref")
	}

	// target: a registry gantry pushes into, or an engine that pulls.
	if req.Target == "" {
		return nil, nil, nil, fmt.Errorf("job has nothing to do: set `target`")
	}
	ex.dst, err = resolveDest(w.stores, req.Target)
	if err != nil {
		return nil, nil, nil, z.Err(err, "target")
	}

	// The target-side reference is derived from the TAG (before any digest
	// pinning below), so the target stays tag-named; only the source pull is
	// anchored to the verified digest.
	platforms := req.Platforms
	var transfers []*Transfer
	var asDigests []name.Digest
	switch d := ex.dst.(type) {
	case pusher:
		if len(req.As) > 0 {
			return nil, nil, nil, fmt.Errorf("`as` names the image on an engine; store %q is a registry", d.Name())
		}
		ex.cacheRef, err = d.dstRef(ex.srcRef)
		if err != nil {
			return nil, nil, nil, z.Err(err, "rewrite into %q", d.Name())
		}
		// A digest-named cache ref keeps the source digest, so a copy-mode
		// commit must preserve the source manifest/index byte-for-byte — a
		// rebuilt (platform-filtered) index has a different digest and the
		// registry would reject the put. Proxy mode commits nothing (the cache
		// resolves the digest itself), so it is exempt.
		isDigestDst := false
		if _, ok := ex.cacheRef.(name.Digest); ok {
			rd, isRegistry := ex.dst.(*registryDest)
			isDigestDst = !isRegistry || !rd.isProxy()
		}
		if isDigestDst {
			if len(req.Platforms) > 0 {
				return nil, nil, nil, fmt.Errorf("a digest-pinned copy preserves the source image verbatim (all platforms); omit platforms")
			}
			ex.verbatim = true
		}
		transfers = append(transfers, &Transfer{
			Store: d.Name(), Kind: d.Kind(), Source: ex.source.Name,
			Ref: ex.cacheRef.Name(), State: "pending",
		})
	case puller:
		// An engine pulls exactly one platform; an empty request means the
		// daemon host's own. The value is handed to the daemon as-is — a
		// platform the image lacks is the daemon's error, surfaced by the pull.
		if len(platforms) > 1 {
			return nil, nil, nil, fmt.Errorf("engine destination %q pulls a single platform; got %d", d.Name(), len(platforms))
		}
		if len(platforms) == 0 {
			p, err := d.hostPlatform(ctx)
			if err != nil {
				return nil, nil, nil, z.Err(err, "resolve host platform of %q", d.Name())
			}
			platforms = []string{p}
		}
		ex.platform = platforms[0]
		ex.pullRef, err = d.pullRef(ex.srcRef, ex.source)
		if err != nil {
			return nil, nil, nil, err
		}
		// The image is recorded under the requested names — so a cache-fed
		// engine can keep the upstream name — instead of the pull reference.
		// The strings are kept VERBATIM: containerd resolves image names by
		// exact match, so normalizing (docker.io -> index.docker.io) would
		// break kubelet lookups. A digest reference is admitted too — it is
		// validated against the job's pinned digest below, once verification
		// has had its say.
		for _, n := range req.As {
			parsed, err := name.ParseReference(n, w.srcOpts...)
			if err != nil {
				return nil, nil, nil, z.Err(err, "parse `as` name %q", n)
			}
			if d, ok := parsed.(name.Digest); ok {
				asDigests = append(asDigests, d)
				ex.asDigest = true
			}
			ex.as = append(ex.as, n)
		}
		transfers = append(transfers, &Transfer{
			Store: d.Name(), Kind: d.Kind(), Source: ex.source.Name,
			Ref: ex.pullRef, State: "pending",
		})
	default:
		return nil, nil, nil, fmt.Errorf("store %q can neither be pushed to nor pull", ex.dst.Name())
	}

	// Verify the source signature (fail-closed) before admitting the job, and pin
	// ex.srcRef to the verified digest so the move covers exactly what was verified.
	// Runs after the target ref is derived from the tag (see above).
	verified := false
	if w.verifier != nil {
		res, err := w.verifier.Verify(ctx, ex.source, ex.srcRef)
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
			if rd, ok := ex.dst.(*registryDest); ok && rd.isProxy() {
				return nil, nil, nil, fmt.Errorf("signature verification requires a copy-mode destination; store %q is proxy", rd.Name())
			}
			ex.srcRef = ex.srcRef.Context().Digest(dg.String())
			log.From(w.rootCtx()).Info("source signature verified",
				slog.String("ref", req.Ref), slog.String("source", ex.source.Name), slog.String("digest", dg.String()))
		}
	}

	// A digest `as` name is honest only when it names exactly what the engine
	// pulls: it requires an anchored pull (the job ref is a digest, or
	// verification pinned one) and must carry that anchor digest — anything
	// else would register a reference to content that is not that digest.
	if len(asDigests) > 0 {
		dg, ok := ex.srcRef.(name.Digest)
		if !ok {
			return nil, nil, nil, fmt.Errorf("digest `as` names require a digest-pinned job (a digest ref, or a verified source)")
		}
		for _, d := range asDigests {
			if d.DigestStr() != dg.DigestStr() {
				return nil, nil, nil, fmt.Errorf("`as` name %q does not carry the job's pinned digest %s", d.Name(), dg.DigestStr())
			}
		}
	}

	// Referrer propagation (signatures travel with the image): copy the source's
	// referrer artifacts into the cache with the source digest preserved — which
	// requires copying the image verbatim, i.e. every platform. Registry
	// destinations only: an engine pull has no referrer transport.
	rd, isRegistry := ex.dst.(*registryDest)
	if req.CopyReferrers != nil && *req.CopyReferrers && !isRegistry {
		return nil, nil, nil, fmt.Errorf("copy_referrers requires a registry destination; store %q is an engine", ex.dst.Name())
	}
	if isRegistry {
		if req.CopyReferrers != nil {
			ex.copyReferrers = *req.CopyReferrers
		} else {
			// Default on only when this job actually verified a signature and the
			// request did not narrow the platform set: the pinned digest still
			// protects a narrowed copy, only signature propagation is skipped.
			ex.copyReferrers = verified && !rd.isProxy() && len(req.Platforms) == 0
		}
		if ex.copyReferrers {
			if rd.isProxy() {
				return nil, nil, nil, fmt.Errorf("copy_referrers requires a copy-mode destination; store %q is proxy", rd.Name())
			}
			if len(req.Platforms) > 0 {
				return nil, nil, nil, fmt.Errorf("copy_referrers preserves the source image verbatim (all platforms); omit platforms or set copy_referrers to false")
			}
			platforms = nil // the verbatim commit needs every child manifest present
			ex.verbatim = true
		}
	}

	return ex, transfers, platforms, nil
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

func (w *Copier) execute(ctx context.Context, job *Job) error {
	w.store.Update(job.ID, func(j *Job) {
		j.State = JobRunning
		j.DateStarted = time.Now()
	})
	ex := job.exec

	// Dispatch on what the destination can do, not what it is: a pusher is
	// filled by gantry's copy pipeline, a puller fetches the image itself.
	switch d := ex.dst.(type) {
	case pusher:
		return w.runCopy(ctx, job, ex, d)
	case puller:
		return w.runPull(ctx, job, ex, d)
	default:
		return fmt.Errorf("store %q can neither be pushed to nor pull", ex.dst.Name())
	}
}

// runCopy fills a pusher destination (the registry transfer). Its failure fails
// the job.
func (w *Copier) runCopy(ctx context.Context, job *Job, ex *jobExec, d pusher) error {
	t := job.Transfers[0]
	w.store.Update(job.ID, func(*Job) { t.State = "running" })

	src, err := d.newSource(ex.source)
	if err != nil {
		return w.failTransfer(job, t, err)
	}
	plan, err := src.Resolve(ctx, ex.srcRef, ex.cacheRef, job.Platforms)
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
	committed, err := src.Commit(ctx, ex.srcRef, ex.cacheRef, job.Platforms, ex.verbatim)
	if err != nil {
		return w.failTransfer(job, t, z.Err(err, "commit"))
	}
	ex.committed = committed
	if ex.copyReferrers {
		// copy_referrers is only ever planned for a registry destination, and
		// registryDest is the only pusher; the assertion makes that dependency loud.
		rd := d.(*registryDest)
		n, err := copyReferrers(ctx, ex.source, rd.cfg, ex.srcRef, committed, ex.cacheRef.Context())
		if err != nil {
			// Fail closed: the signatures are the point of referrer propagation.
			return w.failTransfer(job, t, z.Err(err, "copy referrers"))
		}
		log.From(ctx).Info("referrers copied",
			slog.Int("count", n), slog.String("subject", committed.String()), slog.String("store", d.Name()))
	}
	w.store.Update(job.ID, func(*Job) {
		if committed != (v1.Hash{}) {
			t.Digest = committed.String()
		}
		t.State = "done"
	})
	log.From(ctx).Info("cache filled",
		slog.String("store", d.Name()), slog.String("ref", ex.cacheRef.Name()), slog.Int64("bytes", t.BytesDone.Load()))
	return nil
}

// runPull tells a puller destination (an engine daemon) to fetch the image. The
// transfer total is estimated upfront from the source manifest (best-effort) and
// refined by the daemon's own progress reports as they arrive; the daemon's
// error — including an unavailable platform — fails the job as-is.
func (w *Copier) runPull(ctx context.Context, job *Job, ex *jobExec, d puller) error {
	t := job.Transfers[0]
	// Anchor the pull when the source is digest-pinned (a verified job, or a
	// digest ref): the daemon pulls repo@digest and tags it as the ref.
	digest := ""
	if dg, ok := ex.srcRef.(name.Digest); ok {
		digest = dg.DigestStr()
	}
	w.store.Update(job.ID, func(*Job) {
		t.State = "running"
		t.Digest = digest
	})

	// Size estimate from the source manifest; the daemon's layer reports replace
	// it with actual figures as they arrive.
	if plan, err := upstreamPlan(ctx, ex.source, ex.srcRef, []string{ex.platform}); err == nil {
		w.store.Update(job.ID, func(*Job) { t.BytesTotal = plan.Total })
	} else {
		log.From(ctx).Debug("pull size estimate unavailable",
			slog.String("ref", ex.srcRef.Name()), slog.String("error", err.Error()))
	}

	// Digest-named `as` references are backed by the anchor manifest's raw
	// bytes, fetched from the job's SOURCE (the cache) — the origin registry is
	// never contacted. Fetched before the pull so a cache that cannot resolve
	// the digest fails the job before any bytes move; the engine registers the
	// names only after its pull succeeded (a name registered over absent
	// content would send `docker run` back to the registry in the name).
	var anchor *down.AnchorBlob
	if ex.asDigest {
		dg, ok := ex.srcRef.(name.Digest)
		if !ok {
			return w.failTransfer(job, t, fmt.Errorf("digest `as` names require an anchored pull"))
		}
		a, err := fetchAnchor(ctx, ex.source, dg)
		if err != nil {
			return w.failTransfer(job, t, err)
		}
		anchor = a
	}

	sink := &engineSink{w: w, jobID: job.ID, t: t, idx: map[string]*LayerProgress{}}
	recorded, err := d.pull(ctx, ex.pullRef, digest, ex.platform, ex.as, anchor, sink)
	if err != nil {
		return w.failTransfer(job, t, err)
	}
	w.store.Update(job.ID, func(*Job) {
		var tot int64
		for _, lp := range t.Layers {
			if lp.State != "exists" {
				lp.State = "done"
				lp.Done.Store(lp.Total)
			}
			tot += lp.Total
		}
		if len(t.Layers) > 0 {
			// The daemon's own layer totals supersede the upstream estimate.
			t.BytesTotal = tot
		}
		t.BytesDone.Store(t.BytesTotal)
		t.State = "done"
	})
	if w.pullHook != nil {
		// Stamp the names the daemon ACTUALLY records, as reported by the
		// engine — not the requested ones: a classic-store docker skips digest
		// `as` names, and stamping a name the daemon never held would leave the
		// image unowned (reaped as untagged) while the index claims otherwise.
		names := recorded
		if len(names) == 0 {
			names = []string{ex.pullRef}
		}
		for _, n := range names {
			w.pullHook(d.Name(), n)
		}
	}
	log.From(ctx).Info("engine pulled",
		slog.String("store", d.Name()), slog.String("ref", ex.pullRef), slog.String("platform", ex.platform))
	return nil
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

func (w *Copier) copyLayers(ctx context.Context, job *Job, t *Transfer, src Source, plan *Plan, ex *jobExec) error {
	c := w.wc.MaxConcurrentLayers
	if c < 1 {
		c = 1
	}
	sem := make(chan struct{}, c)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	srcRepo := ex.srcRef.Context()
	dstRepo := ex.cacheRef.Context()
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
			if err := src.Fill(ctx, srcRepo, dstRepo, pl, sink); err != nil {
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
			// Only an error that IS the cancellation counts as canceled. A layer
			// failure cancels job.ctx to abort its sibling copies, but the job must
			// be recorded as failed with the real error, not silently "canceled".
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
