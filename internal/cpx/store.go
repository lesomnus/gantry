package cpx

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// memStore is an ephemeral, in-memory Store. Scalar fields are guarded by mu;
// atomic byte counters on Job/LayerProgress are updated lock-free.
//
// An execution (*Job) is addressed by one or more handles: coalesced submits
// share a single *Job but each holds a distinct handle carrying its own labels
// and cancel state. A job's originating (primary) handle shares the job's id,
// so a single-caller job reads and writes exactly like a plain record. Extra
// handles from coalescing get their own ids and resolve through h.jobID.
type memStore struct {
	mu      sync.RWMutex
	jobs    map[string]*Job    // executions, keyed by execution id
	handles map[string]*handle // per-caller handles, keyed by handle id
	idem    map[string]string  // client idempotency key -> handle id
}

// handle is a per-caller view onto a shared execution. Labels and timing are
// per-handle; state and progress read through to the execution.
type handle struct {
	id        string
	jobID     string
	labels    map[string]string
	createdAt time.Time
	canceled  bool      // this caller canceled; the execution may still run for others
	endedAt   time.Time // when this handle was canceled
}

func NewMemStore() Store {
	return &memStore{jobs: map[string]*Job{}, handles: map[string]*handle{}, idem: map[string]string{}}
}

func (s *memStore) Add(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; ok {
		return fmt.Errorf("job %q already exists", j.ID)
	}
	if _, ok := s.handles[j.ID]; ok {
		return fmt.Errorf("job %q already exists", j.ID)
	}
	j.refs, j.pins = 1, 1
	s.jobs[j.ID] = j
	s.handles[j.ID] = &handle{
		id:        j.ID,
		jobID:     j.ID,
		labels:    j.Labels,
		createdAt: j.DateCreated,
	}
	return nil
}

func (s *memStore) Attach(key, handleID string, labels map[string]string, createdAt time.Time) (JobSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.activeJob(key)
	if !ok {
		return JobSnapshot{}, false
	}
	h := &handle{id: handleID, jobID: j.ID, labels: labels, createdAt: createdAt}
	s.handles[handleID] = h
	j.refs++
	j.pins++
	return s.handleSnapshot(h)
}

// resolve returns the execution an id refers to, whether the id is a handle's
// or an execution's own. Caller holds mu.
func (s *memStore) resolve(id string) (*Job, bool) {
	if h, ok := s.handles[id]; ok {
		j, ok := s.jobs[h.jobID]
		return j, ok
	}
	j, ok := s.jobs[id]
	return j, ok
}

// activeJob finds a non-terminal, non-canceled execution with the dedup key.
// Caller holds mu.
func (s *memStore) activeJob(key string) (*Job, bool) {
	for _, j := range s.jobs {
		if j.State.Terminal() || j.Canceled() || j.enqueuing || j.sealed {
			// A canceled job is on its way out; a resubmit must start fresh
			// rather than coalesce onto the dying one. An enqueuing job is not
			// yet guaranteed to run, so it is not a coalescing target either.
			// A sealed job is still running but already failing.
			continue
		}
		if j.dedup == key {
			return j, true
		}
	}
	return nil, false
}

// handleSnapshot builds the caller-facing view: the execution's state and
// progress under the handle's id, labels, and submit time. A canceled handle
// reads as canceled even while the execution runs on for others. Caller holds mu.
func (s *memStore) handleSnapshot(h *handle) (JobSnapshot, bool) {
	j, ok := s.jobs[h.jobID]
	if !ok {
		return JobSnapshot{}, false
	}
	snap := j.snapshot()
	snap.ID = h.id
	snap.Labels = h.labels
	snap.DateCreated = h.createdAt
	if h.canceled {
		snap.State = JobCanceled
		snap.Err = ""
		snap.DateEnded = h.endedAt
	}
	return snap, true
}

func (s *memStore) Job(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolve(id)
}

func (s *memStore) Snapshot(id string) (JobSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Keyed on handles only: an id whose handle was erased is gone for the
	// caller, even if a coalesced sibling keeps the shared execution alive
	// under the same (former primary) id — resolving it here would resurrect
	// an erased job. The worker addresses the execution through Update instead.
	if h, ok := s.handles[id]; ok {
		return s.handleSnapshot(h)
	}
	return JobSnapshot{}, false
}

func (s *memStore) List(f Filter) []JobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]JobSnapshot, 0, len(s.handles))
	for _, h := range s.handles {
		snap, ok := s.handleSnapshot(h)
		if !ok {
			continue
		}
		if f.State != "" && snap.State != f.State {
			continue
		}
		if f.Ref != "" && !strings.Contains(snap.Ref, f.Ref) {
			continue
		}
		if !f.Since.IsZero() && snap.DateCreated.Before(f.Since) {
			continue
		}
		if !matchLabels(snap.Labels, f.Labels) {
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].DateCreated.After(out[k].DateCreated) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// matchLabels reports whether have carries every key/value pair in want.
func matchLabels(have, want map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}

func (s *memStore) Active(key string) (JobSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if j, ok := s.activeJob(key); ok {
		return j.snapshot(), true
	}
	return JobSnapshot{}, false
}

func (s *memStore) Filling(ref string, exclude string) (<-chan struct{}, bool) {
	if ref == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.ID == exclude {
			// A routed job publishes the reference its own later hop reads. Waiting
			// for itself could only ever burn the whole bound.
			continue
		}
		// Same exclusions as coalescing: a terminal, canceled or already-failing
		// job will never produce the ref, and an enqueuing one is not yet
		// guaranteed to run, so waiting on any of them would only burn the
		// caller's timeout.
		if j.State.Terminal() || j.Canceled() || j.enqueuing || j.sealed || j.done == nil {
			continue
		}
		for _, f := range j.Fills {
			if f == ref {
				return j.done, true
			}
		}
	}
	return nil, false
}

func (s *memStore) Counts() map[JobState]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[JobState]int, 5)
	for _, j := range s.jobs {
		out[j.State]++
	}
	return out
}

func (s *memStore) Update(id string, fn func(*Job)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.resolve(id)
	if !ok {
		return false
	}
	fn(j)
	return true
}

func (s *memStore) RetrySource(id string) (Request, JobState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handles[id]
	if !ok {
		return Request{}, "", false
	}
	j, ok := s.jobs[h.jobID]
	if !ok {
		return Request{}, "", false
	}
	req := j.req
	req.Labels = h.labels // resubmit under THIS caller's labels, not the originator's
	if h.canceled {
		return req, JobCanceled, true // honor the caller's own canceled view
	}
	return req, j.State, true
}

func (s *memStore) Cancel(id string) (JobSnapshot, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.handles[id]
	if !ok {
		return JobSnapshot{}, false, false
	}
	j, ok := s.jobs[h.jobID]
	if !ok {
		return JobSnapshot{}, false, false
	}
	if h.canceled || j.State.Terminal() {
		snap, _ := s.handleSnapshot(h)
		return snap, true, true
	}
	h.canceled = true
	h.endedAt = time.Now()
	j.refs--
	if j.refs <= 0 {
		j.Cancel() // last caller gone: stop the shared work
	}
	snap, _ := s.handleSnapshot(h)
	return snap, true, false
}

func (s *memStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeHandle(id)
}

// removeHandle drops a handle record and, when it was the execution's last
// handle, cancels and evicts the execution. Caller holds mu.
func (s *memStore) removeHandle(id string) bool {
	h, ok := s.handles[id]
	if !ok {
		return false
	}
	delete(s.handles, id)
	j, ok := s.jobs[h.jobID]
	if !ok {
		return true
	}
	if !h.canceled {
		j.refs--
		if j.refs <= 0 {
			j.Cancel()
		}
	}
	j.pins--
	if j.pins <= 0 {
		j.Cancel() // release the execution's context, mirroring the old Delete
		delete(s.jobs, j.ID)
	}
	return true
}

func (s *memStore) Sweep(now time.Time, ttl time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, h := range s.handles {
		ended, isEnded := s.handleEnd(h)
		if isEnded && !ended.IsZero() && now.Sub(ended) > ttl {
			s.removeHandle(id) // deleting the current key mid-range is safe
			n++
		}
	}
	for key, id := range s.idem {
		if _, ok := s.handles[id]; !ok {
			delete(s.idem, key) // key retention rides on the handle record's TTL
		}
	}
	return n
}

// handleEnd reports when a handle reached its terminal view, if it has: its own
// cancel time, or the execution's end. Caller holds mu.
func (s *memStore) handleEnd(h *handle) (time.Time, bool) {
	if h.canceled {
		return h.endedAt, true
	}
	if j, ok := s.jobs[h.jobID]; ok && j.State.Terminal() {
		return j.DateEnded, true
	}
	return time.Time{}, false
}

func (s *memStore) Remember(key, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idem[key] = id
}

func (s *memStore) Idem(key string) (JobSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.idem[key]
	if !ok {
		return JobSnapshot{}, false
	}
	h, ok := s.handles[id]
	if !ok {
		return JobSnapshot{}, false // handle swept; treat as a miss
	}
	return s.handleSnapshot(h)
}
