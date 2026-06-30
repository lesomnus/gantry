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
	"github.com/lesomnus/otx"
	"github.com/lesomnus/z"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// metrics are the otel instruments for the warm pipeline. They route to the
// configured meter provider, or to a no-op when none is set.
type metrics struct {
	bytes    metric.Int64Counter
	duration metric.Float64Histogram
	active   metric.Int64UpDownCounter
}

func newMetrics(ctx context.Context) *metrics {
	m := otx.Meter(ctx)
	bytes, _ := m.Int64Counter("gantry.warm.bytes", metric.WithUnit("By"),
		metric.WithDescription("bytes moved into the cache"))
	duration, _ := m.Float64Histogram("gantry.warm.duration", metric.WithUnit("s"),
		metric.WithDescription("warm job duration"))
	active, _ := m.Int64UpDownCounter("gantry.warm.jobs.active",
		metric.WithDescription("warm jobs in flight"))
	return &metrics{bytes: bytes, duration: duration, active: active}
}

// ErrQueueFull is returned by Submit when the pending-job buffer is saturated.
var ErrQueueFull = errors.New("warm queue is full")

// Distributor triggers downstream targets to pull a warmed cache reference.
// It is wired in once the downstream seam exists; a nil Distributor skips the
// fan-out step.
type Distributor interface {
	Distribute(ctx context.Context, job *Job, store Store)
}

// Request is a warm submission.
type Request struct {
	Ref               string
	Platforms         []string
	Targets           []string
	TriggerDownstream *bool // nil = config default
}

// Warmer runs a two-tier worker pool: `jobs` workers each warm one image, and
// within a job up to `concurrency` blobs are moved at once.
type Warmer struct {
	src   Source
	store Store
	rc    config.RegistryConfig
	wc    config.WarmConfig
	dist  Distributor

	jobs    chan *Job
	idgen   func() string
	srcOpts []name.Option // upstream parse options (tests inject name.Insecure)
	metrics *metrics

	base context.Context
	wg   sync.WaitGroup
}

func NewWarmer(src Source, store Store, rc config.RegistryConfig, wc config.WarmConfig) *Warmer {
	q := wc.QueueSize
	if q < 1 {
		q = 1
	}
	return &Warmer{
		src:     src,
		store:   store,
		rc:      rc,
		wc:      wc,
		jobs:    make(chan *Job, q),
		idgen:   newID,
		metrics: newMetrics(context.Background()),
	}
}

// SetDistributor wires the downstream fan-out. Call before Start.
func (w *Warmer) SetDistributor(d Distributor) { w.dist = d }

// Start launches the worker pool bound to ctx; workers stop when Stop is called.
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

// Stop closes the queue and waits for in-flight jobs to drain. The caller is
// expected to cancel the base context first so blocked network calls abort.
func (w *Warmer) Stop() {
	close(w.jobs)
	w.wg.Wait()
}

// Submit validates and enqueues a warm request, collapsing onto an existing
// in-flight job with the same image+platform set.
func (w *Warmer) Submit(req Request) (JobSnapshot, error) {
	src, err := name.ParseReference(req.Ref, w.srcOpts...)
	if err != nil {
		return JobSnapshot{}, z.Err(err, "parse ref %q", req.Ref)
	}
	platforms := w.effectivePlatforms(req.Platforms)

	key := dedupKey(src.Name(), platforms)
	if snap, ok := w.store.Active(key); ok {
		return snap, nil
	}

	dst, err := Rewrite(w.rc.Rewrite, w.rc.Host, src, w.rc.Insecure)
	if err != nil {
		return JobSnapshot{}, z.Err(err, "rewrite ref")
	}

	id := w.idgen()
	ctx, cancel := context.WithCancel(w.base)
	job := NewJob(id, src.Name(), dst.Name(), platforms, time.Now())
	job.ctx = ctx
	job.cancel = cancel
	job.src = src
	job.dst = dst
	job.trigger = w.shouldTrigger(req.TriggerDownstream)
	job.reqTargets = req.Targets
	if err := w.store.Add(job); err != nil {
		cancel()
		return JobSnapshot{}, err
	}

	select {
	case w.jobs <- job:
		snap, _ := w.store.Snapshot(id)
		return snap, nil
	default:
		w.store.Delete(id) // roll back; queue is saturated
		return JobSnapshot{}, ErrQueueFull
	}
}

func (w *Warmer) worker() {
	defer w.wg.Done()
	for job := range w.jobs {
		w.run(job)
	}
}

func (w *Warmer) run(job *Job) {
	ctx, span := otx.TraceStart(job.ctx, "warm.job", trace.WithAttributes(
		attribute.String("gantry.ref", job.Ref),
		attribute.String("gantry.cache_ref", job.CacheRef),
	))
	defer span.End()
	w.metrics.active.Add(ctx, 1)
	defer w.metrics.active.Add(ctx, -1)

	start := time.Now()
	err := w.pipeline(ctx, job)
	w.finish(job, err)

	state := "done"
	if err != nil {
		state = "failed"
		span.RecordError(err)
	}
	attrs := metric.WithAttributes(attribute.String("state", state))
	w.metrics.duration.Record(ctx, time.Since(start).Seconds(), attrs)
	w.metrics.bytes.Add(ctx, job.BytesDone.Load())
}

// pipeline runs resolve -> warm -> commit -> distribute, returning the first error.
func (w *Warmer) pipeline(ctx context.Context, job *Job) error {
	w.store.Update(job.ID, func(j *Job) {
		j.State = JobPulling
		j.StartedAt = time.Now()
	})

	plan, err := w.src.Resolve(ctx, job.src, job.dst, job.Platforms)
	if err != nil {
		return err
	}
	w.store.Update(job.ID, func(j *Job) {
		j.BytesTotal = plan.Total
		for _, pl := range plan.Layers {
			j.Layers = append(j.Layers, &LayerProgress{
				Digest:   pl.Digest,
				Platform: pl.Platform,
				Total:    pl.Size,
				State:    "pending",
			})
		}
	})

	if err := w.warmLayers(ctx, job, plan); err != nil {
		return err
	}
	if err := w.src.Commit(ctx, job.src, job.dst, job.Platforms); err != nil {
		return z.Err(err, "commit")
	}
	w.store.Update(job.ID, func(j *Job) { j.State = JobWarm })

	if w.dist != nil && job.trigger {
		w.store.Update(job.ID, func(j *Job) { j.State = JobTriggering })
		w.dist.Distribute(ctx, job, w.store)
	}
	return nil
}

func (w *Warmer) warmLayers(ctx context.Context, job *Job, plan *Plan) error {
	c := w.wc.MaxConcurrentLayers
	if c < 1 {
		c = 1
	}
	sem := make(chan struct{}, c)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	src := job.src.Context()
	dst := job.dst.Context()
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
			sink := &layerSink{w: w, job: job, lp: lp}
			if err := w.src.Warm(ctx, src, dst, pl, sink); err != nil {
				w.store.Update(job.ID, func(*Job) { lp.State = "failed" })
				once.Do(func() {
					firstErr = err
					job.Cancel() // abort sibling blobs
				})
			}
		}(plan.Layers[i], job.Layers[i])
	}
	wg.Wait()
	return firstErr
}

// finish moves the job to a terminal state. A nil error means success; a
// context cancellation maps to canceled, anything else to failed.
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

func (w *Warmer) effectivePlatforms(req []string) []string {
	if len(req) > 0 {
		return req
	}
	if len(w.wc.Platforms) > 0 {
		return w.wc.Platforms
	}
	return []string{runtime.GOOS + "/" + runtime.GOARCH}
}

func (w *Warmer) shouldTrigger(req *bool) bool {
	if req != nil {
		return *req
	}
	return w.wc.TriggerDownstream
}

// layerSink reports one blob's progress: byte deltas are lock-free atomics,
// state transitions go through the store lock.
type layerSink struct {
	w   *Warmer
	job *Job
	lp  *LayerProgress
}

func (s *layerSink) Add(n int64) {
	s.lp.Done.Add(n)
	s.job.BytesDone.Add(n)
}

func (s *layerSink) SetState(state string) {
	s.w.store.Update(s.job.ID, func(*Job) { s.lp.State = state })
}

func newID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("warm: read random: %v", err))
	}
	return "wrm_" + hex.EncodeToString(b[:])
}
