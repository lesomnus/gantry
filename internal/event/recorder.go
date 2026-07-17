package event

// Recorder adapts a Log to the cpx and retention Recorder interfaces, so those
// packages emit audit events without importing the event package's concrete
// types beyond this adapter. Append failures are swallowed: an audit-log write
// must never break the operation it records.
type Recorder struct{ log *Log }

// NewRecorder wraps a Log; nil is safe (every method is a no-op).
func NewRecorder(l *Log) *Recorder { return &Recorder{log: l} }

func (r *Recorder) emit(e Event) {
	if r == nil || r.log == nil {
		return
	}
	_ = r.log.Append(e)
}

// --- cpx.Recorder ---

func (r *Recorder) JobAdmitted(ref, source, target, digest string) {
	r.emit(Event{Type: JobAdmitted, Ref: ref, Store: target, Digest: digest, Detail: kv("source", source)})
}

func (r *Recorder) JobFinished(id, ref, state, errMsg string, bytes int64) {
	r.emit(Event{Type: JobDone, Ref: ref, State: state, Error: errMsg, Detail: kvi(id, bytes)})
}

// --- retention.Recorder ---

func (r *Recorder) GCApplied(store string, deleted, untagged, reaped, errs int) {
	r.emit(Event{Type: GCApplied, Store: store, Detail: gcDetail(deleted, untagged, reaped, errs)})
}

func (r *Recorder) ImageRemoved(store, ref string) {
	r.emit(Event{Type: ImageRemove, Store: store, Ref: ref})
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
