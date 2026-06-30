package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/lesomnus/gantry/internal/warm"
)

type createJobRequest struct {
	Ref        string   `json:"ref"`
	Platforms  []string `json:"platforms"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Distribute []string `json:"distribute"`
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	snap, err := s.warmer.Submit(warm.Request{
		Ref:        req.Ref,
		Platforms:  req.Platforms,
		From:       req.From,
		To:         req.To,
		Distribute: req.Distribute,
	})
	switch {
	case errors.Is(err, warm.ErrQueueFull):
		writeErr(w, http.StatusServiceUnavailable, "job queue is full")
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Location", "/v1/job/"+snap.ID)
	writeJSON(w, http.StatusAccepted, snap)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	f := warm.Filter{
		State: warm.JobState(r.URL.Query().Get("state")),
		Ref:   r.URL.Query().Get("ref"),
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.store.List(f)})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.store.Snapshot(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if !s.store.Delete(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProgress streams progress as Server-Sent Events, or — when ?wait=<dur> is
// given — long-polls and returns a single JSON snapshot once the job is terminal.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.store.Snapshot(id); !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if d, ok := duration(r.URL.Query().Get("wait")); ok {
		s.longPoll(w, r, id, d)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		snap, ok := s.store.Snapshot(id)
		if !ok {
			return
		}
		writeSSE(w, "progress", snap)
		flusher.Flush()
		if snap.State.Terminal() {
			writeSSE(w, "done", map[string]string{"state": string(snap.State)})
			flusher.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) longPoll(w http.ResponseWriter, r *http.Request, id string, wait time.Duration) {
	deadline := time.Now().Add(wait)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		snap, ok := s.store.Snapshot(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "job not found")
			return
		}
		if snap.State.Terminal() || time.Now().After(deadline) {
			writeJSON(w, http.StatusOK, snap)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}
