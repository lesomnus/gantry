package event

import (
	"log/slog"
	"sync/atomic"
)

// Recorder adapts a Log to the cpx and retention Recorder interfaces, so those
// packages emit audit events without importing the event package's concrete
// types beyond this adapter. An audit-log write must never break the operation
// it records, so an Append failure is not propagated — but it is surfaced: each
// failure is logged at WARN with the running dropped count, so a silently
// failing log (full disk, corrupt/locked db) is observable.
type Recorder struct {
	log     *Log
	logger  *slog.Logger
	dropped atomic.Uint64
}

// NewRecorder wraps a Log; a nil Log is safe (every method is a no-op). A nil
// logger falls back to slog.Default().
func NewRecorder(l *Log, logger *slog.Logger) *Recorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{log: l, logger: logger}
}

func (r *Recorder) emit(e Event) {
	if r == nil || r.log == nil {
		return
	}
	if err := r.log.Append(e); err != nil {
		n := r.dropped.Add(1)
		r.logger.Warn("audit event dropped: append to the log failed",
			slog.String("type", string(e.Type)),
			slog.String("error", err.Error()),
			slog.Uint64("dropped_total", n))
	}
}

// --- cpx.Recorder ---

func (r *Recorder) JobAdmitted(id, ref, source, target, digest string) {
	r.emit(Event{Type: JobAdmitted, Ref: ref, Store: target, Digest: digest, Detail: jobDetail(id, source)})
}

// JobFellBack records that a job left the store it was pointed at: `from` could
// not serve it, `to` was tried instead, and cause is why. Emitted when the
// second attempt STARTS, so it is present even if that one fails too — the fact
// worth keeping is that the intended source did not deliver.
func (r *Recorder) JobFellBack(id, ref, from, to, cause string) {
	r.emit(Event{Type: JobFallback, Ref: ref, Store: to, Error: cause, Detail: jobDetail(id, from)})
}

func (r *Recorder) JobFinished(id, ref, state, errMsg string, bytes int64) {
	r.emit(Event{Type: JobDone, Ref: ref, State: state, Error: errMsg, Detail: kvi(id, bytes)})
}

// --- retention.Recorder ---

func (r *Recorder) GCApplied(store string, deleted, untagged, reaped, errs int) {
	r.emit(Event{Type: GCApplied, Store: store, Detail: gcDetail(deleted, untagged, reaped, errs)})
}

func (r *Recorder) ImageRemoved(store, ref, digest, reason string) {
	r.emit(Event{Type: ImageRemove, Store: store, Ref: ref, Digest: digest, Detail: removeDetail(reason)})
}

func (r *Recorder) Pinned(store, value string, unpin bool) {
	t := Pinned
	if unpin {
		t = Unpinned
	}
	r.emit(Event{Type: t, Store: store, Ref: value})
}

// --- server-side manual ops ---

func (r *Recorder) ImagePulled(store, ref string) {
	r.emit(Event{Type: ImagePulled, Store: store, Ref: ref})
}
