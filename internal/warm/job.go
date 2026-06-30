// Package warm holds the image-move domain: jobs, the transfers that make them
// up, the job store, and the Warmer engine. A job moves an image between stores;
// each step (a registry copy, or an engine pull) is a Transfer with the same
// per-layer progress shape.
package warm

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

func (s JobState) Terminal() bool {
	switch s {
	case JobDone, JobFailed, JobCanceled:
		return true
	default:
		return false
	}
}

// Job is the live record of one image move. Its Transfers are filled in by the
// Warmer when the job is planned and run.
type Job struct {
	ID        string
	Ref       string // the requested image reference
	Platforms []string

	State     JobState
	Err       string
	Transfers []*Transfer

	CreatedAt time.Time
	StartedAt time.Time
	EndedAt   time.Time

	// Set by the Warmer at submit time; not serialized.
	ctx    context.Context
	cancel context.CancelFunc
	dedup  string
	exec   *jobExec
}

func NewJob(id, ref string, platforms []string, now time.Time) *Job {
	return &Job{
		ID:        id,
		Ref:       ref,
		Platforms: platforms,
		State:     JobPending,
		CreatedAt: now,
	}
}

func (j *Job) SetCancel(fn context.CancelFunc) { j.cancel = fn }

func (j *Job) Cancel() {
	if j.cancel != nil {
		j.cancel()
	}
}

func (j *Job) DedupKey() string { return j.dedup }

// dedupKey collapses identical moves (same image, platforms, route) onto one job.
func dedupKey(ref string, platforms []string, from, to string, distribute []string) string {
	ps := append([]string(nil), platforms...)
	sort.Strings(ps)
	ds := append([]string(nil), distribute...)
	sort.Strings(ds)
	return strings.Join([]string{ref, strings.Join(ps, ","), from, to, strings.Join(ds, ",")}, "\x00")
}

// Transfer is one step of a job: moving an image into a store. A registry copy
// (gantry-driven) and an engine pull (daemon-driven) share this shape.
type Transfer struct {
	Store string // destination store name (or host)
	Kind  string // registry | docker | containerd
	From  string // source store/host
	Ref   string // the reference placed in the destination

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
	State    string // pending | pulling | done | exists | failed
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
	ID        string             `json:"id"`
	Ref       string             `json:"ref"`
	Platforms []string           `json:"platforms"`
	State     JobState           `json:"state"`
	Err       string             `json:"error"`
	Transfers []TransferSnapshot `json:"transfers"`
	CreatedAt time.Time          `json:"created_at"`
	StartedAt time.Time          `json:"started_at,omitempty"`
	EndedAt   time.Time          `json:"ended_at,omitempty"`
}

type TransferSnapshot struct {
	Store      string          `json:"store"`
	Kind       string          `json:"kind"`
	From       string          `json:"from"`
	Ref        string          `json:"ref"`
	State      string          `json:"state"`
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
	State    string `json:"state"`
}

func (j *Job) snapshot() JobSnapshot {
	s := JobSnapshot{
		ID:        j.ID,
		Ref:       j.Ref,
		Platforms: j.Platforms,
		State:     j.State,
		Err:       j.Err,
		CreatedAt: j.CreatedAt,
		StartedAt: j.StartedAt,
		EndedAt:   j.EndedAt,
	}
	for _, t := range j.Transfers {
		ts := TransferSnapshot{
			Store:      t.Store,
			Kind:       t.Kind,
			From:       t.From,
			Ref:        t.Ref,
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
	State JobState
	Ref   string
}

// Store holds jobs. The in-memory implementation is ephemeral; the interface
// lets a persistent backend drop in without touching the API layer.
type Store interface {
	Add(j *Job) error
	Job(id string) (*Job, bool)
	Snapshot(id string) (JobSnapshot, bool)
	List(f Filter) []JobSnapshot
	Active(key string) (JobSnapshot, bool)
	Update(id string, fn func(*Job)) bool
	Delete(id string) bool
	Sweep(now time.Time, ttl time.Duration) int
}
