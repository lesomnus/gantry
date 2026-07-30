// Package cpx holds the image-move domain: jobs, the transfers that make them
// up, the job store, and the Copier engine. A job moves an image between stores;
// each step (a registry copy, or an engine pull) is a Transfer with the same
// per-layer progress shape.
package cpx

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type JobState string

const (
	JobPending  JobState = "pending"
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobFailed   JobState = "failed"
	JobCanceled JobState = "canceled"
)

// Valid reports whether s is a known state (for API filter validation).
func (s JobState) Valid() bool {
	switch s {
	case JobPending, JobRunning, JobDone, JobFailed, JobCanceled:
		return true
	default:
		return false
	}
}

func (s JobState) Terminal() bool {
	switch s {
	case JobDone, JobFailed, JobCanceled:
		return true
	default:
		return false
	}
}

// VerificationSnapshot reports the admission-time signature verification: the
// audit primitive tying a mutable tag request to the exact digest that was
// verified and moved.
type VerificationSnapshot struct {
	Mode     string `json:"mode" enums:"off,verify-if-present,require"` // effective mode for the source store
	Verified bool   `json:"verified"`                                   // a signature was actually verified
	Digest   string `json:"digest,omitempty"`                           // the digest the job was pinned to
}

// Job is the live record of one image move. Its Transfers are filled in by the
// Copier when the job is planned and run.
type Job struct {
	ID        string
	Ref       string // the requested image reference
	Platforms []string
	As        []string // engine dest: names the image is recorded under
	// FallbackToOrigin is the effective decision for this job (request value, or
	// the server default): an engine pull its source could not serve is
	// re-attempted against the registry named in Ref.
	FallbackToOrigin bool
	// RequireAuthority is the effective decision for this job: refuse rather than
	// read a nearer cache of a source that could not confirm the reference.
	RequireAuthority bool
	// Fills are the target-side references this job's steps publish into their
	// stores — the things another job's read could be waiting on. Empty for a job
	// whose only step is an engine pull, which publishes nothing another job reads.
	// Released together when the job ends, so a waiter on an intermediate is freed
	// one hop later than strictly necessary.
	Fills []string
	// Source and Target are the stores the job was ADMITTED for: what the caller
	// asked, resolved to store names. They are deliberately not derived from the
	// transfers. A transfer says where some bytes actually came from, and there can
	// be several of those for one job — alternatives when a source could not serve
	// it, and one per step when gantry routes the move through another store — so
	// no single row answers "what was this job for".
	Source string
	Target string
	// Labels is the seed metadata of the job's originating handle; per-caller
	// labels live on the handles (see the store), so a coalesced move keeps
	// each caller's own set. Not used to run the move, only to find it.
	Labels map[string]string

	State        JobState
	Err          string
	Verification *VerificationSnapshot // nil when verification is disabled
	Transfers    []*Transfer

	DateCreated time.Time
	DateStarted time.Time
	DateEnded   time.Time

	// Set by the Copier at submit time; not serialized.
	ctx      context.Context
	cancel   context.CancelFunc
	dedup    string
	exec     *execPlan
	req      Request     // the original request, for Retry's fresh re-plan
	canceled atomic.Bool // Cancel was requested; Active must not coalesce onto it
	// sealed marks a still-running execution that has already failed and is only
	// winding down. It is not a state — the job's outcome is decided by its error
	// — but coalescing must skip it, or a racing identical submit would be handed
	// this failure instead of a fresh move. Guarded by the store mutex.
	sealed bool
	// done is closed once the execution has finished, whatever its outcome. It
	// is the signal another job waits on when it needs this one's output; unlike
	// ctx it means "finished", not "abandon the work" — the store cancels ctx
	// when the last handle goes, which says nothing about the work being over.
	// Closed by run() so an erased or evicted record still releases its waiters.
	done     chan struct{}
	doneOnce sync.Once

	// refs and pins track the handles pointing at this shared execution, both
	// guarded by the store mutex. refs counts handles that still want the move
	// to run (a canceled handle drops out); reaching zero cancels the work.
	// pins counts handle records that reference it (canceled ones included);
	// reaching zero evicts the record. See memStore.
	refs int
	pins int
	// enqueuing marks the brief window between publishing a Submit-created job
	// and confirming it onto the run queue; while set, coalescing skips it so a
	// racing identical submit cannot attach to a job that may still fail to
	// enqueue and be rolled back. Guarded by the store mutex; zero value (a
	// directly-added job) is immediately coalesceable.
	enqueuing bool
}

func NewJob(id, ref string, platforms []string, now time.Time) *Job {
	return &Job{
		ID:          id,
		Ref:         ref,
		Platforms:   platforms,
		State:       JobPending,
		DateCreated: now,
		done:        make(chan struct{}),
	}
}

// Done is closed when the execution finishes. A job built outside NewJob has no
// channel and reads as never finishing; Filling skips such records rather than
// handing a waiter something that can only time out.
func (j *Job) Done() <-chan struct{} { return j.done }

// markDone releases everyone waiting on this execution. Safe to call more than
// once and from any goroutine.
func (j *Job) markDone() {
	if j.done == nil {
		return
	}
	j.doneOnce.Do(func() { close(j.done) })
}

func (j *Job) SetCancel(fn context.CancelFunc) { j.cancel = fn }

func (j *Job) Cancel() {
	j.canceled.Store(true)
	if j.cancel != nil {
		j.cancel()
	}
}

// Canceled reports whether Cancel was requested (the terminal state may lag).
func (j *Job) Canceled() bool { return j.canceled.Load() }

func (j *Job) DedupKey() string { return j.dedup }

// dedupKey collapses identical moves (same image, platforms, route) onto one job —
// an interchangeable move. What a caller may be SERVED is part of it, not just what
// is moved: `fallback` because a submit that refused the origin must not be handed
// a job allowed to read from it (nor the reverse, which would silently drop the
// fallback the caller asked for), and `strictAuthority` because a submit that
// refused unconfirmed content must not be handed a job that read a cache the
// authority never vouched for.
//
// The route itself is deliberately NOT in the key: every route delivers the same
// image to the same store, so it is not identity-bearing, and two submits that
// probed differently are still interchangeable.
func dedupKey(ref string, platforms []string, source, target string, as []string, fallback, strictAuthority bool) string {
	ps := append([]string(nil), platforms...)
	sort.Strings(ps)
	ns := append([]string(nil), as...)
	sort.Strings(ns)
	return strings.Join([]string{
		ref, strings.Join(ps, ","), source, target, strings.Join(ns, ","),
		strconv.FormatBool(fallback), strconv.FormatBool(strictAuthority),
	}, "\x00")
}

// Transfer is one ATTEMPT at one step of a job: moving an image into a store. A
// registry copy (gantry-driven) and an engine pull (daemon-driven) share this
// shape.
//
// Rows sharing a Step are alternatives — the step needed any one of them — so a
// failed row followed by a done row of the same step is one source that could not
// serve the image followed by one that could. Rows with different Steps are
// consecutive hops, each of which had to happen.
type Transfer struct {
	Step   int    // which step of the job's plan this row belongs to
	Store  string // target store name (or host)
	Kind   string // oci | docker | containerd
	Source string // source store/host
	Ref    string // the reference placed in the target
	// Digest is the manifest/index digest this transfer is anchored to: the
	// digest the registry copy committed, and the digest an engine pull was
	// pinned to. Empty when the step ran by tag alone.
	Digest string

	State      string // pending | running | done | exists | failed
	BytesTotal int64
	BytesDone  atomic.Int64
	Layers     []*LayerProgress
	Err        string
}

// LayerProgress tracks one blob within a transfer.
type LayerProgress struct {
	Digest   string
	Platform string
	Total    int64
	Done     atomic.Int64
	State    string // pending | pulling | copied | done | exists | failed
}

// Plan is the manifest walk a registry Source produces before bytes move.
type Plan struct {
	Ref    string
	Layers []PlannedLayer
	Total  int64
}

type PlannedLayer struct {
	Digest   string
	Repo     string
	Platform string
	Size     int64
}

// --- snapshots (immutable views for the API) ---

type JobSnapshot struct {
	ID        string   `json:"id"`
	Ref       string   `json:"ref"`
	Platforms []string `json:"platforms"`
	As        []string `json:"as,omitempty"`
	// Source and Target are the stores the job was admitted for — the request,
	// not whichever transfer served it.
	Source string `json:"source,omitempty"`
	Target string `json:"target,omitempty"`
	// FallbackToOrigin and RequireAuthority are the effective decisions this job
	// ran under.
	FallbackToOrigin bool                  `json:"fallback_to_origin"`
	RequireAuthority bool                  `json:"require_authority"`
	Labels           map[string]string     `json:"labels,omitempty"`
	State            JobState              `json:"state"`
	Err              string                `json:"error"`
	Verification     *VerificationSnapshot `json:"verification,omitempty"`
	Transfers        []TransferSnapshot    `json:"transfers"`
	DateCreated      time.Time             `json:"date_created"`
	DateStarted      time.Time             `json:"date_started,omitempty"`
	DateEnded        time.Time             `json:"date_ended,omitempty"`
}

type TransferSnapshot struct {
	Step       int             `json:"step"` // hop this attempt belongs to; see Transfer
	Store      string          `json:"store"`
	Kind       string          `json:"kind" enums:"oci,docker,containerd"` // which store kind ran this step
	Source     string          `json:"source"`
	Ref        string          `json:"ref"`
	Digest     string          `json:"digest,omitempty"`                                 // manifest/index digest the step was anchored to
	State      string          `json:"state" enums:"pending,running,done,exists,failed"` // transfer step state
	BytesTotal int64           `json:"bytes_total"`
	BytesDone  int64           `json:"bytes_done"`
	Layers     []LayerSnapshot `json:"layers"`
	Err        string          `json:"error,omitempty"`
}

type LayerSnapshot struct {
	Digest   string `json:"digest"`
	Platform string `json:"platform"`
	Total    int64  `json:"total"`
	Done     int64  `json:"done"`
	State    string `json:"state" enums:"pending,pulling,copied,done,exists,failed"` // per-layer progress state
}

func (j *Job) snapshot() JobSnapshot {
	s := JobSnapshot{
		ID:               j.ID,
		Ref:              j.Ref,
		Platforms:        j.Platforms,
		As:               j.As,
		Source:           j.Source,
		Target:           j.Target,
		FallbackToOrigin: j.FallbackToOrigin,
		RequireAuthority: j.RequireAuthority,
		Labels:           j.Labels,
		State:            j.State,
		Err:              j.Err,
		DateCreated:      j.DateCreated,
		DateStarted:      j.DateStarted,
		DateEnded:        j.DateEnded,
	}
	if j.Verification != nil {
		v := *j.Verification
		s.Verification = &v
	}
	for _, t := range j.Transfers {
		ts := TransferSnapshot{
			Step:       t.Step,
			Store:      t.Store,
			Kind:       t.Kind,
			Source:     t.Source,
			Ref:        t.Ref,
			Digest:     t.Digest,
			State:      t.State,
			BytesTotal: t.BytesTotal,
			BytesDone:  t.BytesDone.Load(),
			Err:        t.Err,
		}
		for _, l := range t.Layers {
			ts.Layers = append(ts.Layers, LayerSnapshot{
				Digest:   l.Digest,
				Platform: l.Platform,
				Total:    l.Total,
				Done:     l.Done.Load(),
				State:    l.State,
			})
		}
		s.Transfers = append(s.Transfers, ts)
	}
	return s
}

// Filter narrows List results.
type Filter struct {
	State  JobState
	Ref    string
	Since  time.Time         // only jobs created at/after this instant
	Labels map[string]string // subset match: a job must carry every pair
	Limit  int               // truncate after sorting (newest first); 0 = no limit
}

// Store holds jobs. The in-memory implementation is ephemeral; the interface
// lets a persistent backend drop in without touching the API layer.
//
// A job is a shared execution addressed by one or more per-caller handles: an
// identical submit coalesces onto the running job but gets its own handle, so
// its labels and its cancel stay independent. Handle ids and execution ids
// share one namespace and every method that takes an id resolves through it —
// Add seeds a job with a primary handle whose id equals the job's, so a
// single-caller job behaves exactly like a plain record.
type Store interface {
	// Add registers a new execution and its primary handle (both keyed by j.ID).
	Add(j *Job) error
	// Attach adds a handle to an in-flight execution matching key, returning
	// that handle's snapshot. It is how an identical submit coalesces without
	// losing the caller's labels. ok=false when no such execution is active.
	Attach(key, handleID string, labels map[string]string, createdAt time.Time) (JobSnapshot, bool)
	Job(id string) (*Job, bool)
	Snapshot(id string) (JobSnapshot, bool)
	List(f Filter) []JobSnapshot
	// Counts tallies executions by state without materializing snapshots
	// (polled by the metrics observer on every collection).
	Counts() map[JobState]int
	Active(key string) (JobSnapshot, bool)
	// Filling reports an active execution whose target-side reference is ref —
	// a job that is, right now, putting exactly this image into the store some
	// other job wants to read it from. The returned channel closes when that
	// execution finishes, whatever its outcome; ok=false when nothing is filling
	// ref. Used to wait out a cache that is merely not filled YET rather than
	// treating it as a miss. exclude is the asking job's own id: a routed job
	// publishes the very reference its own later hop reads, and waiting for itself
	// could only ever time out.
	Filling(ref string, exclude string) (done <-chan struct{}, ok bool)
	Update(id string, fn func(*Job)) bool
	// RetrySource returns the request to resubmit for a handle and that
	// handle's effective (per-caller) state, so a retry honors the caller's own
	// canceled view and carries the caller's own labels rather than the
	// originating submit's. ok=false when no such handle exists.
	RetrySource(id string) (req Request, state JobState, ok bool)
	// Cancel detaches the handle and, when it was the execution's last active
	// handle, stops the shared work; the record is kept. already reports the
	// handle was already canceled or the execution already terminal.
	Cancel(id string) (snap JobSnapshot, ok bool, already bool)
	Delete(id string) bool
	Sweep(now time.Time, ttl time.Duration) int

	// Remember maps a client idempotency key to a handle; Idem returns that
	// handle while its record survives, letting a retried submit return the same
	// job instead of re-running the move.
	Remember(key, id string)
	Idem(key string) (JobSnapshot, bool)
}
