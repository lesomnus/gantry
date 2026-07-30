package cpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/z"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// execute runs a job's plan: its steps in order, each step trying its attempts
// until one delivers. There is no branch on what kind of store a step targets or
// on why a step exists — the plan already answered both — so this loop is the
// whole of gantry's execution control flow.
func (w *Copier) execute(ctx context.Context, job *Job) error {
	w.store.Update(job.ID, func(j *Job) {
		j.State = JobRunning
		j.DateStarted = time.Now()
	})
	p := job.exec

	delivered := make(map[int]bool, len(p.steps))
	var errs []error
	for _, st := range p.steps {
		err := w.runStep(ctx, job, p, st, delivered)
		if err == nil {
			delivered[st.idx] = true
			continue
		}
		errs = append(errs, err)
		if !st.optional {
			return errors.Join(errs...)
		}
		// A step gantry added for its own benefit: its failure is on the record and
		// the job carries on, because the caller asked for the last step and a route
		// that does not work is not a failure.
		log.From(ctx).Warn("abandoning a step gantry added for itself",
			slog.String("job", job.ID), slog.Int("step", st.idx),
			slog.String("store", st.dst.Name()), slog.String("error", err.Error()))
	}
	if !delivered[p.last().idx] {
		// Unreachable while the last step is required, which validate() enforces.
		return errors.Join(append(errs, fmt.Errorf("no step delivered the image"))...)
	}
	return nil
}

// runStep tries a step's attempts in order until one delivers. Each attempt gets
// its own Transfer row, so an attempt that could not serve the image stays on the
// record of a job that nevertheless completed.
func (w *Copier) runStep(ctx context.Context, job *Job, p *execPlan, st *execStep, delivered map[int]bool) error {
	var errs []error
	var prev *execAttempt
	waited := false

	for _, at := range st.attempts {
		if !satisfied(at.needs, delivered) {
			// The route this attempt would have read was not filled, so reading it
			// would only fail. Not an error — the next attempt is the alternative —
			// but it is exactly the fact "a cache gantry chose is not being used",
			// which is what the counter and the audit event exist to surface.
			log.From(ctx).Info("giving up on a source whose route did not run",
				slog.String("job", job.ID), slog.Int("step", st.idx), slog.String("source", at.src.Name))
			w.recordGivingUp(ctx, job, at, next(st.attempts, at), fmt.Errorf("the hop that would have filled %s did not deliver", at.src.Name))
			continue
		}
		if prev != nil {
			w.recordGivingUp(ctx, job, prev, at, errs[len(errs)-1])
		}
		for {
			err := w.runAttempt(ctx, job, st, at)
			if err == nil {
				return nil
			}
			errs = append(errs, z.Err(err, "from %s", at.src.Name))
			if !worthAnotherSource(ctx, err) {
				return errors.Join(errs...)
			}
			// A source gets one second chance, and only against a job that was
			// filling it while this attempt ran — otherwise nothing has changed and
			// retrying is pure waste.
			if waited || !w.waitForFill(ctx, job, at.waitFill) {
				break
			}
			waited = true
		}
		prev = at
	}
	if len(errs) == 0 {
		// Every attempt was pruned: the step had nothing to try.
		return fmt.Errorf("step %d: no attempt was runnable", st.idx)
	}
	return errors.Join(errs...)
}

// runAttempt performs one attempt against its own freshly published row.
func (w *Copier) runAttempt(ctx context.Context, job *Job, st *execStep, at *execAttempt) error {
	m, err := st.newMover(w, at)
	if err != nil {
		return err
	}
	t := w.publishRow(job, st, at)
	w.store.Update(job.ID, func(*Job) { t.State = "running" })

	if err := m.run(ctx, job, t); err != nil {
		return w.failTransfer(job, t, err)
	}
	w.store.Update(job.ID, func(j *Job) {
		t.State = "done"
		// A failed attempt may have sealed the job against new callers while it was
		// winding down. It recovered, so lift that: an identical submit should join
		// this move rather than start a second copy of work already done.
		j.sealed = false
	})
	return nil
}

// publishRow gives an attempt the row it reports into. A step's first attempt
// claims the row already published for that step, when there is one, so a job
// that runs exactly as planned looks exactly as it did before this existed;
// anything else appends. Rows are therefore always in execution order.
func (w *Copier) publishRow(job *Job, st *execStep, at *execAttempt) *Transfer {
	var t *Transfer
	w.store.Update(job.ID, func(j *Job) {
		for _, c := range j.Transfers {
			if c.Step == st.idx && c.State == "pending" {
				// Claim the seeded row, correcting the source: the seed names the
				// step's first attempt, which is not necessarily the one that runs.
				c.Source, c.Ref = at.src.Name, st.rowRef(at)
				t = c
				return
			}
		}
		t = &Transfer{
			Step: st.idx, Store: st.dst.Name(), Kind: st.dst.Kind(),
			Source: at.src.Name, Ref: st.rowRef(at), State: "pending",
		}
		j.Transfers = append(j.Transfers, t)
	})
	return t
}

// satisfied reports whether every step an attempt needs has delivered.
func satisfied(needs []int, delivered map[int]bool) bool {
	for _, i := range needs {
		if !delivered[i] {
			return false
		}
	}
	return true
}

// next is the attempt after at, or nil when it is the last.
func next(attempts []*execAttempt, at *execAttempt) *execAttempt {
	for i, c := range attempts {
		if c == at && i+1 < len(attempts) {
			return attempts[i+1]
		}
	}
	return nil
}

// recordGivingUp reports that a source gantry meant to use is not being used. The
// reason names WHAT WAS GIVEN UP, not what is being taken up: abandoning a cache
// gantry chose for itself (`route`) is a different operational fact from leaving
// the source the caller named for the origin (`origin`), and it is the former that
// says "the cache is not earning its keep".
//
// It is deliberately not derived from counting rows — a wait-for-fill retry adds a
// row without consuming an attempt, so counting desynchronises — and it fires for
// an attempt gantry skipped as well as one that failed, because a route that never
// ran is exactly as unused as one that failed.
func (w *Copier) recordGivingUp(ctx context.Context, job *Job, from, to *execAttempt, cause error) {
	reason := string(from.why)
	toName := "(none)"
	if to != nil {
		toName = to.src.Name
	}
	w.metrics.fallback.Add(ctx, 1, metric.WithAttributes(
		attribute.String("from", from.src.Name),
		attribute.String("to", toName),
		attribute.String("reason", reason)))
	msg := cause.Error()
	log.From(ctx).Warn("giving up on a source",
		slog.String("job", job.ID), slog.String("ref", job.Ref),
		slog.String("from", from.src.Name), slog.String("to", toName),
		slog.String("reason", reason), slog.String("error", msg))
	if w.rec != nil {
		w.rec.JobFellBack(job.ID, job.Ref, from.src.Name, toName, msg)
	}
}
