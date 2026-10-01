package cpx

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/xport"
	"github.com/lesomnus/otx/log"
)

// replayCheckTimeout bounds the one request StillDelivered makes, so a target
// that does not answer delays a replay by this much rather than holding the
// submit open.
const replayCheckTimeout = 5 * time.Second

// StillDelivered reports whether a finished job's target still holds what the
// job delivered, so an idempotent replay does not answer "done" for a result
// that has since gone — removed by a registry's own GC, or by hand (#35).
//
// Only a job that ended DONE is asked about; anything else replays as it is.
// It answers false only when the target said, definitely, that the result is
// not there: a registry that answered 404 for the digest, an engine that does
// not know the name. A target that could not be asked, or a job whose record
// does not say what to ask for, replays as before — re-running on doubt would
// turn every unreachable target into a new job.
func (w *Copier) StillDelivered(ctx context.Context, snap JobSnapshot) bool {
	if snap.State != JobDone {
		return true
	}
	tr, ok := finalTransfer(snap)
	if !ok {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, replayCheckTimeout)
	defer cancel()

	held, known := w.targetHolds(ctx, snap, tr)
	if known && !held {
		log.From(ctx).Info("an idempotent replay's result is gone from its target; running the job again",
			slog.String("job", snap.ID), slog.String("store", tr.Store), slog.String("ref", tr.Ref), slog.String("digest", tr.Digest))
	}
	return !known || held
}

// finalTransfer is the transfer that delivered into the job's target: the
// last hop's attempt that succeeded.
func finalTransfer(snap JobSnapshot) (TransferSnapshot, bool) {
	last := -1
	for _, t := range snap.Transfers {
		last = max(last, t.Step)
	}
	for i := len(snap.Transfers) - 1; i >= 0; i-- {
		t := snap.Transfers[i]
		if t.Step == last && (t.State == "done" || t.State == "exists") {
			return t, true
		}
	}
	return TransferSnapshot{}, false
}

// targetHolds asks the target whether it holds the delivery. known is false
// when it could not be asked, or the job's record does not say what to ask.
func (w *Copier) targetHolds(ctx context.Context, snap JobSnapshot, tr TransferSnapshot) (held, known bool) {
	cfg, ok := w.stores.Config(tr.Store)
	if !ok {
		return false, false
	}
	if cfg.IsRegistry() {
		ref, err := name.ParseReference(tr.Ref, w.refOpts(cfg)...)
		if err != nil {
			return false, false
		}
		if tr.Digest != "" {
			// The digest, not the tag: the tag may since name something else, and
			// what the job delivered is that manifest.
			ref = ref.Context().Digest(tr.Digest)
		}
		rt, err := xport.Transport(cfg)
		if err != nil {
			return false, false
		}
		if _, err := remote.Head(ref, baseOpts(ctx, registryAuth(cfg), rt)...); err != nil {
			if absent(err) {
				return false, true
			}
			return false, false
		}
		return true, true
	}
	eng, err := w.stores.Engine(tr.Store)
	if err != nil {
		return false, false
	}
	h, ok := eng.(down.Holder)
	if !ok {
		return false, false
	}
	// The names the job left on the engine: the ones asked for, or the pull's
	// own when none were.
	names := snap.As
	if len(names) == 0 {
		names = []string{tr.Ref}
	}
	for _, n := range names {
		has, err := h.Has(ctx, n)
		if err != nil {
			return false, false
		}
		if !has {
			return false, true
		}
	}
	return true, true
}
