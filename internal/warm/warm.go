package warm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/z"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ErrQueueFull is returned by Submit when the pending-job buffer is saturated.
var ErrQueueFull = errors.New("warm queue is full")

// metrics are the otel instruments for the job pipeline (no-op when unconfigured).
type metrics struct {
	bytes    metric.Int64Counter
	duration metric.Float64Histogram
	active   metric.Int64UpDownCounter
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

// Request is a job submission: move Ref from store From into store To, then have
// the Distribute engines pull it. From/To are registry stores (name or host);
// Distribute are engine stores. To may be empty (engines pull from From directly).
type Request struct {
	Ref        string
	Platforms  []string
	From       string
	To         string
	Distribute []string
}

// engineStep is one resolved downstream pull.
type engineStep struct {
	transfer int // index into Job.Transfers
	engine   down.Engine
	ref      string
}

// jobExec is the resolved plan for a job, computed at submit time.
type jobExec struct {
	from     config.StoreConfig
	to       config.StoreConfig
	hasCache bool
	src      name.Reference
	dst      name.Reference // valid only when hasCache
	cacheIdx int            // index of the registry transfer, or -1
	engines  []engineStep
}

// Warmer runs a two-tier worker pool: MaxConcurrentJobs jobs at once, each moving
// up to MaxConcurrentLayers layers at once during its registry copy.
type Warmer struct {
	stores *store.Set
	store  Store
	wc     config.WarmConfig

	jobs    chan *Job
	idgen   func() string
	srcOpts []name.Option // parse options for the source ref (tests inject name.Insecure)
	metrics *metrics

	base context.Context
	wg   sync.WaitGroup
}

func NewWarmer(stores *store.Set, jobStore Store, wc config.WarmConfig) *Warmer {
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
	}
}

func (w *Warmer) Start(ctx context.Context) {
	w.base = ctx
	w.metrics = newMetrics(ctx)
	n := w.wc.MaxConcurrentJobs
	if n < 1 {
		n = 1
	}
	for range n {
		w.wg.Add(1)
		go w.worker()
	}
}

func (w *Warmer) Stop() {
	close(w.jobs)
	w.wg.Wait()
}

// Submit resolves and enqueues a job, collapsing identical in-flight moves.
func (w *Warmer) Submit(req Request) (JobSnapshot, error) {
	ex, transfers, platforms, err := w.plan(req)
	if err != nil {
		return JobSnapshot{}, err
	}

	key := dedupKey(req.Ref, platforms, ex.from.Name, ex.to.Name, req.Distribute)
	if snap, ok := w.store.Active(key); ok {
		return snap, nil
	}

	id := w.idgen()
	ctx, cancel := context.WithCancel(w.base)
	job := NewJob(id, req.Ref, platforms, time.Now())
	job.ctx, job.cancel, job.dedup, job.exec = ctx, cancel, key, ex
	job.Transfers = transfers
	if err := w.store.Add(job); err != nil {
		cancel()
		return JobSnapshot{}, err
	}

	select {
	case w.jobs <- job:
		snap, _ := w.store.Snapshot(id)
		return snap, nil
	default:
		w.store.Delete(id)
		return JobSnapshot{}, ErrQueueFull
	}
}

// plan resolves the request into an execution plan and the initial transfer rows.
func (w *Warmer) plan(req Request) (*jobExec, []*Transfer, []string, error) {
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
			Store: ex.to.Name, Kind: "registry", From: ex.from.Name,
			Ref: ex.dst.Name(), State: "pending",
		})
	}

	// distribute targets (engines)
	names := req.Distribute
	if names == nil && w.wc.DistributeByDefault {
		names = w.stores.EngineNames()
	}
	pullBase := ex.src
	if ex.hasCache {
		pullBase = ex.dst
	}
	for _, n := range names {
		eng, err := w.stores.Engine(n)
		if err != nil {
			return nil, nil, nil, z.Err(err, "distribute")
		}
		ref, err := w.pullRef(n, pullBase, ex)
		if err != nil {
			return nil, nil, nil, err
		}
		ex.engines = append(ex.engines, engineStep{transfer: len(transfers), engine: eng, ref: ref})
		transfers = append(transfers, &Transfer{
			Store: n, Kind: eng.Kind(), From: transferFrom(ex), Ref: ref, State: "pending",
		})
	}

	if len(transfers) == 0 {
		return nil, nil, nil, fmt.Errorf("job has nothing to do: set `to` and/or `distribute`")
	}
	return ex, transfers, platforms, nil
}

func transferFrom(ex *jobExec) string {
	if ex.hasCache {
		return ex.to.Name
	}
	return ex.from.Name
}

// pullRef computes the reference an engine is told to pull, applying the engine's
// pull_host or the cache store's downstream_host override.
func (w *Warmer) pullRef(engineName string, base name.Reference, ex *jobExec) (string, error) {
	host := ""
	if c, ok := w.stores.Config(engineName); ok {
		host = c.PullHost
	}
	if host == "" && ex.hasCache {
		host = ex.to.DownstreamHost
	}
	if host == "" {
		return base.Name(), nil
	}
	return rewriteHost(base, host)
}

func (w *Warmer) worker() {
	defer w.wg.Done()
	for job := range w.jobs {
		w.run(job)
	}
}

func (w *Warmer) run(job *Job) {
	ctx, span := otx.TraceStart(job.ctx, "job", trace.WithAttributes(attribute.String("gantry.ref", job.Ref)))
	defer span.End()
	w.metrics.active.Add(ctx, 1)
	defer w.metrics.active.Add(ctx, -1)

	start := time.Now()
	err := w.execute(ctx, job)
	w.finish(job, err)

	state := "done"
	if err != nil {
		state = "failed"
		span.RecordError(err)
	}
	w.metrics.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("state", state)))
	w.metrics.bytes.Add(ctx, jobBytes(job))
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
	if len(ex.engines) > 0 {
		w.runDistribute(ctx, job, ex)
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
	if err := src.Commit(ctx, ex.src, ex.dst, job.Platforms); err != nil {
		return w.failTransfer(job, t, z.Err(err, "commit"))
	}
	w.store.Update(job.ID, func(*Job) { t.State = "done" })
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

// runDistribute triggers each engine to pull concurrently. An engine failure
// marks only its transfer; the job still completes.
func (w *Warmer) runDistribute(ctx context.Context, job *Job, ex *jobExec) {
	var wg sync.WaitGroup
	for _, step := range ex.engines {
		wg.Add(1)
		go func(step engineStep) {
			defer wg.Done()
			t := job.Transfers[step.transfer]
			w.store.Update(job.ID, func(*Job) { t.State = "running" })
			sink := &engineSink{w: w, jobID: job.ID, t: t, idx: map[string]*LayerProgress{}}
			err := step.engine.Pull(ctx, step.ref, sink)
			w.store.Update(job.ID, func(*Job) {
				if err != nil {
					t.State = "failed"
					t.Err = err.Error()
					return
				}
				for _, lp := range t.Layers {
					if lp.State != "exists" {
						lp.State = "done"
						lp.Done.Store(lp.Total)
					}
				}
				t.BytesDone.Store(t.BytesTotal)
				t.State = "done"
			})
		}(step)
	}
	wg.Wait()
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
		case errors.Is(err, context.Canceled), errors.Is(job.ctx.Err(), context.Canceled):
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

// engineSink folds an engine's per-layer reports into a Transfer, upserting
// layers by digest and recomputing the transfer totals.
type engineSink struct {
	w     *Warmer
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

func identifier(ref name.Reference) string {
	if d, ok := ref.(name.Digest); ok {
		return "@" + d.DigestStr()
	}
	return ":" + ref.Identifier()
}

// rewriteHost replaces the registry host of ref, preserving repo path and tag/digest.
func rewriteHost(ref name.Reference, host string) (string, error) {
	repo := ref.Context().RepositoryStr()
	var out string
	if d, ok := ref.(name.Digest); ok {
		out = host + "/" + repo + "@" + d.DigestStr()
	} else {
		out = host + "/" + repo + ":" + ref.Identifier()
	}
	if _, err := name.ParseReference(out); err != nil {
		return "", z.Err(err, "invalid downstream ref %q", out)
	}
	return out, nil
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
