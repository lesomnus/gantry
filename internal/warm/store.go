package warm

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// memStore is an ephemeral, in-memory Store. Scalar fields are guarded by mu;
// atomic byte counters on Job/LayerProgress are updated lock-free.
type memStore struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

func NewMemStore() Store {
	return &memStore{jobs: map[string]*Job{}}
}

func (s *memStore) Add(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; ok {
		return fmt.Errorf("job %q already exists", j.ID)
	}
	s.jobs[j.ID] = j
	return nil
}

func (s *memStore) Job(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

func (s *memStore) Snapshot(id string) (JobSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return JobSnapshot{}, false
	}
	return j.snapshot(), true
}

func (s *memStore) List(f Filter) []JobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]JobSnapshot, 0, len(s.jobs))
	for _, j := range s.jobs {
		if f.State != "" && j.State != f.State {
			continue
		}
		if f.Ref != "" && !strings.Contains(j.Ref, f.Ref) {
			continue
		}
		out = append(out, j.snapshot())
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	return out
}

func (s *memStore) Active(key string) (JobSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.State.Terminal() {
			continue
		}
		if j.DedupKey() == key {
			return j.snapshot(), true
		}
	}
	return JobSnapshot{}, false
}

func (s *memStore) Update(id string, fn func(*Job)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false
	}
	fn(j)
	return true
}

func (s *memStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false
	}
	j.Cancel()
	delete(s.jobs, id)
	return true
}

func (s *memStore) Sweep(now time.Time, ttl time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, j := range s.jobs {
		if j.State.Terminal() && !j.EndedAt.IsZero() && now.Sub(j.EndedAt) > ttl {
			j.Cancel() // release the job's context, mirroring Delete
			delete(s.jobs, id)
			n++
		}
	}
	return n
}
