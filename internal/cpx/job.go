// Package cpx holds the image-move domain: jobs, the transfers that make them
// up, the job store, and the Copier engine. A job moves an image between stores;
// each step (a registry copy, or an engine pull) is a Transfer with the same
// per-layer progress shape.
package cpx

import (
	"context"
	"sort"
	"strings"
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
	exec     *jobExec
	req      Request     // the original request, for Retry's fresh re-plan
	canceled atomic.Bool // Cancel was requested; Active must not coalesce onto it

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
	}
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

// dedupKey collapses identical moves (same image, platforms, route) onto one job.
func dedupKey(ref string, platforms []string, source, target string, as []string) string {
	ps := append([]string(nil), platforms...)
	sort.Strings(ps)
	ns := append([]string(nil), as...)
	sort.Strings(ns)
	return strings.Join([]string{ref, strings.Join(ps, ","), source, target, strings.Join(ns, ",")}, "\x00")
}

// Transfer is one step of a job: moving an image into a store. A registry copy
// (gantry-driven) and an engine pull (daemon-driven) share this shape.
type Transfer struct {
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
	ID           string                `json:"id"`
	Ref          string                `json:"ref"`
	Platforms    []string              `json:"platforms"`
	As           []string              `json:"as,omitempty"`
	Labels       map[string]string     `json:"labels,omitempty"`
	State        JobState              `json:"state"`
	Err          string                `json:"error"`
	Verification *VerificationSnapshot `json:"verification,omitempty"`
	Transfers    []TransferSnapshot    `json:"transfers"`
	DateCreated  time.Time             `json:"date_created"`
	DateStarted  time.Time             `json:"date_started,omitempty"`
	DateEnded    time.Time             `json:"date_ended,omitempty"`
}

type TransferSnapshot struct {
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
		ID:          j.ID,
		Ref:         j.Ref,
		Platforms:   j.Platforms,
		As:          j.As,
		Labels:      j.Labels,
		State:       j.State,
		Err:         j.Err,
		DateCreated: j.DateCreated,
		DateStarted: j.DateStarted,
		DateEnded:   j.DateEnded,
	}
	if j.Verification != nil {
		v := *j.Verification
		s.Verification = &v
	}
	for _, t := range j.Transfers {
		ts := TransferSnapshot{
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
