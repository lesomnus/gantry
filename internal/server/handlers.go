package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
	"github.com/lesomnus/otx/log"
)

type createJobRequest struct {
	Ref        string   `json:"ref" binding:"required" example:"docker.io/library/nginx:1.27"` // Image reference to move (required).
	Platforms  []string `json:"platforms" example:"linux/amd64,linux/arm64"`                   // Platforms to move; defaults to the server platform when empty.
	From       string   `json:"from" example:"dockerhub"`                                      // Source registry store name or host; defaults to the ref's registry.
	To         string   `json:"to" example:"local-cache"`                                      // Destination registry store to copy into; empty means engines pull from `from` directly.
	Distribute []string `json:"distribute" example:"node-a,node-b"`                            // Engine store names that should pull the image afterwards.
}

// handleCreateJob godoc
//
//	@Summary	Create a job
//	@Description	Move an image: copy `from` (oci) into `to` (oci), then have the `distribute` engines pull it. Idempotent per identical move.
//	@Tags		jobs
//	@Accept		json
//	@Produce	json
//	@Param		request	body		createJobRequest	true	"job request"
//	@Success	202		{object}	warm.JobSnapshot
//	@Header		202		{string}	Location	"canonical URL of the created job (/v1/job/{id})"
//	@Failure	400		{object}	errorResponse
//	@Failure	422		{object}	errorResponse	"source image signature verification failed"
//	@Failure	503		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/job [post]
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
		log.From(r.Context()).Warn("job rejected: queue full", slog.String("ref", req.Ref))
		writeErr(w, http.StatusServiceUnavailable, "job queue is full")
		return
	case errors.Is(err, verify.ErrUnsigned), errors.Is(err, verify.ErrUntrusted):
		log.From(r.Context()).Warn("job rejected: signature verification failed",
			slog.String("ref", req.Ref), slog.String("from", req.From), slog.String("error", err.Error()))
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		log.From(r.Context()).Warn("job rejected", slog.String("ref", req.Ref), slog.String("error", err.Error()))
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.From(r.Context()).Info("job accepted",
		slog.String("job", snap.ID), slog.String("ref", req.Ref), slog.String("from", req.From), slog.String("to", req.To))
	w.Header().Set("Location", "/v1/job/"+snap.ID)
	writeJSON(w, http.StatusAccepted, snap)
}

// handleListJobs godoc
//
//	@Summary	List jobs
//	@Tags		jobs
//	@Produce	json
//	@Param		state	query		string	false	"filter by state"
//	@Param		ref		query		string	false	"filter by ref substring"
//	@Success	200		{object}	jobListResponse
//	@Security	BearerAuth
//	@Router		/v1/job [get]
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	f := warm.Filter{
		State: warm.JobState(r.URL.Query().Get("state")),
		Ref:   r.URL.Query().Get("ref"),
	}
	writeJSON(w, http.StatusOK, jobListResponse{Items: s.store.List(f)})
}

// handleGetJob godoc
//
//	@Summary	Get a job
//	@Tags		jobs
//	@Produce	json
//	@Param		id	path		string	true	"job id"
//	@Success	200	{object}	warm.JobSnapshot
//	@Failure	404	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/job/{id} [get]
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.store.Snapshot(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleDeleteJob godoc
//
//	@Summary	Cancel or evict a job
//	@Tags		jobs
//	@Param		id	path	string	true	"job id"
//	@Success	204
//	@Failure	404	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/job/{id} [delete]
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if !s.store.Delete(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProgress godoc
//
//	@Summary	Stream job progress
//	@Description	Streams Server-Sent Events: repeated `event: progress` frames each carrying a JSON warm.JobSnapshot in `data:`, ending with a terminal `event: done` frame. With ?wait=<dur> it instead long-polls and returns a single JSON warm.JobSnapshot (no SSE framing) once the job is terminal or the wait elapses.
//	@Tags		jobs
//	@Produce	text/event-stream
//	@Param		id		path		string	true	"job id"
//	@Param		wait	query		string	false	"long-poll duration, e.g. 30s"
//	@Success	200		{object}	warm.JobSnapshot
//	@Failure	404		{object}	errorResponse
//	@Failure	500		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/job/{id}/progress [get]
//
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
