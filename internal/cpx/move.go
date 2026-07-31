package cpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/z"
)

// mover performs one attempt at one step: get this image into this store, from
// this source, reporting progress into this row. Which of the two exists is
// decided where the step is built, so the executor never asks what kind of store
// it is holding — and a registry copy has no way to reach the engine retention
// hook even by mistake.
type mover interface {
	// run moves the image, or reports why it could not. It must leave t's state
	// alone: the caller owns the row's lifecycle.
	run(ctx context.Context, job *Job, t *Transfer) error
}

// --- registry copy ---------------------------------------------------------

// copyMove fills a pusher destination: gantry pulls each blob from the source and
// pushes it in, then commits the manifest.
type copyMove struct {
	w  *Copier
	st *execStep
	at *execAttempt
	d  pusher
	// committed is the digest the commit landed at, for the referrer copy that
	// follows it.
	committed v1.Hash
}

// newCopyMover is the step's runner factory, bound at plan time to the one place
// that knows this step's destination is pushed into.
func newCopyMover(st *execStep) func(*Copier, *execAttempt) (mover, error) {
	return func(w *Copier, at *execAttempt) (mover, error) {
		d, ok := st.dst.(pusher)
		if !ok {
			return nil, fmt.Errorf("step %d: store %q cannot be pushed to", st.idx, st.dst.Name())
		}
		return &copyMove{w: w, st: st, at: at, d: d}, nil
	}
}

// run copies the image in. Errors leave through attributed(), so a destination
// that refused the write is never mistaken for a source that could not serve it.
func (m *copyMove) run(ctx context.Context, job *Job, t *Transfer) (rerr error) {
	defer func() { rerr = m.attributed(rerr) }()
	w, st, at := m.w, m.st, m.at

	src, err := m.d.newSource(at.src)
	if err != nil {
		return err
	}
	plan, err := src.Resolve(ctx, at.ref, st.ref, st.platforms)
	if err != nil {
		return err
	}
	w.store.Update(job.ID, func(*Job) {
		t.BytesTotal = plan.Total
		for _, pl := range plan.Layers {
			t.Layers = append(t.Layers, &LayerProgress{Digest: pl.Digest, Platform: pl.Platform, Total: pl.Size, State: "pending"})
		}
	})
	if err := w.copyLayers(ctx, job, t, src, plan, at.ref.Context(), st.ref.Context()); err != nil {
		return err
	}
	committed, err := src.Commit(ctx, at.ref, st.ref, st.platforms, st.verbatim)
	if err != nil {
		return z.Err(err, "commit")
	}
	m.committed = committed
	if st.referrers {
		// referrers are only ever planned for a registry destination, and
		// registryDest is the only pusher; the assertion makes that dependency loud.
		rd := m.d.(*registryDest)
		n, err := copyReferrers(ctx, at.src, rd.cfg, at.ref, committed, st.ref.Context())
		if err != nil {
			// Fail closed: the signatures are the point of referrer propagation.
			return z.Err(err, "copy referrers")
		}
		log.From(ctx).Info("referrers copied",
			slog.Int("count", n), slog.String("subject", committed.String()), slog.String("store", m.d.Name()))
	}
	w.store.Update(job.ID, func(*Job) {
		if committed != (v1.Hash{}) {
			t.Digest = committed.String()
		}
	})
	log.From(ctx).Info("registry filled",
		slog.String("store", m.d.Name()), slog.String("source", at.src.Name),
		slog.String("ref", st.ref.Name()), slog.Int64("bytes", t.BytesDone.Load()))
	return nil
}

// attributed marks an error the destination registry itself answered with, so
// the attempt loop does not answer a target-side outage by re-reading the whole
// image from another source — which would be refused identically, and would
// blame the source (and gantry's own cache) in the metric and the audit event
// for a failure that belongs to the job's target.
func (m *copyMove) attributed(err error) error {
	if err == nil || !answeredBy(err, m.st.ref.Context().RegistryStr()) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrDestination, err)
}

// --- engine pull -----------------------------------------------------------

// pullMove tells a puller destination (an engine daemon) to fetch the image. The
// transfer total is estimated upfront from the source manifest (best-effort) and
// refined by the daemon's own progress reports as they arrive.
type pullMove struct {
	w  *Copier
	p  *execPlan
	st *execStep
	at *execAttempt
	d  puller
}

func newPullMover(p *execPlan, st *execStep) func(*Copier, *execAttempt) (mover, error) {
	return func(w *Copier, at *execAttempt) (mover, error) {
		d, ok := st.dst.(puller)
		if !ok {
			return nil, fmt.Errorf("step %d: store %q does not pull", st.idx, st.dst.Name())
		}
		return &pullMove{w: w, p: p, st: st, at: at, d: d}, nil
	}
}

func (m *pullMove) run(ctx context.Context, job *Job, t *Transfer) error {
	w, p, st, at := m.w, m.p, m.st, m.at

	// Anchor the pull when the plan is digest-pinned (a verified job, or a digest
	// ref): the daemon pulls repo@digest and tags it as the ref.
	digest := ""
	if dg, ok := at.ref.(name.Digest); ok {
		digest = dg.DigestStr()
	}
	w.store.Update(job.ID, func(*Job) { t.Digest = digest })

	// Size estimate from the source manifest; the daemon's layer reports replace it
	// with actual figures as they arrive.
	if plan, err := upstreamPlan(ctx, at.src, at.ref, []string{st.platform()}); err == nil {
		w.store.Update(job.ID, func(*Job) { t.BytesTotal = plan.Total })
	} else {
		log.From(ctx).Debug("pull size estimate unavailable",
			slog.String("ref", at.ref.Name()), slog.String("error", err.Error()))
	}

	// Digest-named `as` references are backed by the anchor manifest's raw bytes,
	// fetched from the store this attempt pulls from — normally the job's source
	// (the cache), so the origin registry is never contacted; on a fallback attempt
	// the anchor comes from the origin along with the content, which is the point
	// of the fallback. Fetched before the pull so a source that cannot resolve the
	// digest fails the attempt before any bytes move; the engine registers the names
	// only after its pull succeeded (a name registered over absent content would
	// send `docker run` back to the registry in the name).
	var anchor *down.AnchorBlob
	if p.asDigest {
		dg, ok := at.ref.(name.Digest)
		if !ok {
			return fmt.Errorf("digest `as` names require an anchored pull")
		}
		a, err := fetchAnchor(ctx, at.src, dg)
		if err != nil {
			return err
		}
		anchor = a
	}

	sink := &engineSink{w: w, jobID: job.ID, t: t, idx: map[string]*LayerProgress{}}
	recorded, err := m.d.pull(ctx, at.pullRef, digest, st.platform(), p.as, anchor, sink)
	if err != nil {
		return err
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
	})
	if w.pullHook != nil {
		// Stamp the names the daemon ACTUALLY holds, as reported by the engine — not
		// the requested ones — so the index never claims a name the daemon does not
		// resolve (which would leave the image reaped as untagged).
		names := recorded
		if len(names) == 0 {
			names = []string{at.pullRef}
		}
		for _, n := range names {
			w.pullHook(m.d.Name(), n)
		}
	}
	log.From(ctx).Info("engine pulled",
		slog.String("store", m.d.Name()), slog.String("source", at.src.Name),
		slog.String("ref", at.pullRef), slog.String("platform", st.platform()))
	return nil
}

// --- attempt classification ------------------------------------------------

// worthAnotherSource reports whether err is worth re-attempting from a different
// place. Cancellation is the job ending, not a source fault; an engine capability
// gap fails identically wherever the bytes come from; and a destination that
// refused the write is not a fault of whoever was reading. Everything else — an
// unreachable host, a missing manifest, even a platform this source's copy of the
// index lacks — is a property of the source, and another one may well not share
// it.
func worthAnotherSource(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	return !errors.Is(err, down.ErrEngine) && !errors.Is(err, ErrDestination)
}
