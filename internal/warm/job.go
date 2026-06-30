// Package warm holds the cache-warming domain: jobs, their progress, the job
// store, and the Source/Warmer engine that pulls images into the cache.
package warm

import (
	"context"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

type JobState string

const (
	JobPending    JobState = "pending"
	JobPulling    JobState = "pulling"
	JobWarm       JobState = "warm"
	JobTriggering JobState = "triggering"
	JobDone       JobState = "done"
	JobFailed     JobState = "failed"
	JobCanceled   JobState = "canceled"
)

// Terminal reports whether the state is final.
func (s JobState) Terminal() bool {
	switch s {
	case JobDone, JobFailed, JobCanceled:
		return true
	default:
		return false
	}
}

// Job is the live, mutable record of one warm request. High-frequency byte
// counters are atomic and updated lock-free by the copy loop; scalar fields are
// mutated only via Store.Update (under the store's write lock).
type Job struct {
	ID        string
	Ref       string // canonical upstream ref, e.g. "docker.io/library/redis:7"
	CacheRef  string // rewrite(Ref): copy push dest / proxy pull src / downstream trigger
	Platforms []string

	State JobState
	Err   string

	BytesTotal int64
	BytesDone  atomic.Int64

	Layers  []*LayerProgress
	Targets []*TargetProgress

	CreatedAt time.Time
	StartedAt time.Time
	EndedAt   time.Time

	// Set by the Warmer when the job is submitted; not serialized.
	ctx        context.Context
	cancel     context.CancelFunc
	src        name.Reference
	dst        name.Reference
	trigger    bool
	reqTargets []string
}

// RequestedTargets returns the downstream targets named in the request; empty
// means "all configured targets". Read by the Distributor.
func (j *Job) RequestedTargets() []string { return j.reqTargets }

// SetRequestedTargets records the requested downstream targets (submit-time setup).
func (j *Job) SetRequestedTargets(names []string) { j.reqTargets = names }

// NewJob creates a pending job. The caller fills Layers/Targets and BytesTotal
// during Resolve, before the job is exposed to readers.
func NewJob(id, ref, cacheRef string, platforms []string, now time.Time) *Job {
	return &Job{
		ID:        id,
		Ref:       ref,
		CacheRef:  cacheRef,
		Platforms: platforms,
		State:     JobPending,
		CreatedAt: now,
	}
}

// SetCancel attaches the cancel func of the job's context so Delete can abort it.
func (j *Job) SetCancel(fn context.CancelFunc) { j.cancel = fn }

// Cancel aborts the job's context if one was attached.
func (j *Job) Cancel() {
	if j.cancel != nil {
		j.cancel()
	}
}

// DedupKey collapses concurrent requests for the same image+platform set onto
// one in-flight job instead of hammering upstream N times.
func (j *Job) DedupKey() string { return dedupKey(j.Ref, j.Platforms) }

func dedupKey(ref string, platforms []string) string {
	ps := append([]string(nil), platforms...)
	sort.Strings(ps)
	return ref + "\x00" + strings.Join(ps, ",")
}

// LayerProgress tracks one blob (layer or config). Done is atomic; State is
// mutated under the store lock.
type LayerProgress struct {
	Digest   string
	Platform string
	Total    int64
	Done     atomic.Int64
	State    string // pending | pulling | warm | exists | failed
}

// TargetProgress tracks one downstream pull.
type TargetProgress struct {
	Name  string
	Kind  string
	Ref   string // the reference this target was told to pull (may differ from CacheRef)
	State string // pending | pulling | pulled | failed
	Err   string
}

// Plan is the manifest walk produced before any bytes move: the full blob set
// and the byte total that becomes Job.BytesTotal.
type Plan struct {
	Ref    string
	Layers []PlannedLayer
	Total  int64
}

type PlannedLayer struct {
	Digest   string
	Repo     string // copy mode: repo path to map into the cache
	Platform string
	Size     int64
}

// JobSnapshot is an immutable view for API responses.
type JobSnapshot struct {
	ID         string           `json:"id"`
	Ref        string           `json:"ref"`
	CacheRef   string           `json:"cache_ref"`
	Platforms  []string         `json:"platforms"`
	State      JobState         `json:"state"`
	Err        string           `json:"error"`
	BytesTotal int64            `json:"bytes_total"`
	BytesDone  int64            `json:"bytes_done"`
	Layers     []LayerSnapshot  `json:"layers"`
	Targets    []TargetSnapshot `json:"targets"`
	CreatedAt  time.Time        `json:"created_at"`
	StartedAt  time.Time        `json:"started_at,omitempty"`
	EndedAt    time.Time        `json:"ended_at,omitempty"`
}

type LayerSnapshot struct {
	Digest   string `json:"digest"`
	Platform string `json:"platform"`
	Total    int64  `json:"total"`
	Done     int64  `json:"done"`
	State    string `json:"state"`
}

type TargetSnapshot struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	State string `json:"state"`
	Err   string `json:"error"`
}

// snapshot copies the job into an immutable view. Callers hold the store's read
// lock for scalar fields; atomic counters are loaded directly.
func (j *Job) snapshot() JobSnapshot {
	s := JobSnapshot{
		ID:         j.ID,
		Ref:        j.Ref,
		CacheRef:   j.CacheRef,
		Platforms:  j.Platforms,
		State:      j.State,
		Err:        j.Err,
		BytesTotal: j.BytesTotal,
		BytesDone:  j.BytesDone.Load(),
		CreatedAt:  j.CreatedAt,
		StartedAt:  j.StartedAt,
		EndedAt:    j.EndedAt,
	}
	for _, l := range j.Layers {
		s.Layers = append(s.Layers, LayerSnapshot{
			Digest:   l.Digest,
			Platform: l.Platform,
			Total:    l.Total,
			Done:     l.Done.Load(),
			State:    l.State,
		})
	}
	for _, t := range j.Targets {
		s.Targets = append(s.Targets, TargetSnapshot{
			Name:  t.Name,
			Kind:  t.Kind,
			Ref:   t.Ref,
			State: t.State,
			Err:   t.Err,
		})
	}
	return s
}

// Filter narrows List results.
type Filter struct {
	State JobState // empty = any
	Ref   string   // substring match on Ref; empty = any
}

// Store holds warm jobs. The in-memory implementation is ephemeral; the
// interface lets a persistent backend drop in without touching the API layer.
type Store interface {
	// Add inserts a new job; it errors if the ID already exists.
	Add(j *Job) error
	// Job returns the live record. Only atomic fields may be mutated on it
	// without going through Update.
	Job(id string) (*Job, bool)
	// Snapshot returns an immutable view for responses.
	Snapshot(id string) (JobSnapshot, bool)
	// List returns snapshots matching the filter, newest first.
	List(f Filter) []JobSnapshot
	// Active returns a non-terminal job sharing the given dedup key.
	Active(key string) (JobSnapshot, bool)
	// Update mutates a job's scalar fields under the write lock.
	Update(id string, fn func(*Job)) bool
	// Delete cancels and removes a job.
	Delete(id string) bool
	// Sweep evicts terminal jobs whose EndedAt is older than ttl.
	Sweep(now time.Time, ttl time.Duration) int
}
