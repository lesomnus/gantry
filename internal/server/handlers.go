package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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
	// Copy the source's referrer artifacts (notation signatures) into `to` with
	// the source digest preserved, so the image still verifies against the cache.
	// Requires copying all platforms (the request must not narrow `platforms`).
	// Defaults to true when verification is enabled and `to` is a copy-mode store.
	CopyReferrers *bool `json:"copy_referrers,omitempty"`
}

// handleCreateJob godoc
//
//	@Summary	Create a job
//	@Description	Move an image: copy `from` (oci) into `to` (oci), then have the `distribute` engines pull it, anchored to the digest the copy committed. Idempotent per identical move.
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
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		// A retried POST after the original finished must return the same job
		// instead of silently re-running the whole move (a mutable tag could by
		// then resolve to a different digest).
		if snap, ok := s.store.Idem(key); ok {
			w.Header().Set("Location", "/v1/job/"+snap.ID)
			writeJSON(w, http.StatusOK, createJobResponse{JobSnapshot: snap, Coalesced: true})
			return
		}
	}
	snap, created, err := s.warmer.Submit(warm.Request{
		Ref:           req.Ref,
		Platforms:     req.Platforms,
		From:          req.From,
		To:            req.To,
		Distribute:    req.Distribute,
		CopyReferrers: req.CopyReferrers,
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
	if created {
		if key := r.Header.Get("Idempotency-Key"); key != "" {
			s.store.Remember(key, snap.ID)
		}
		log.From(r.Context()).Info("job accepted",
			slog.String("job", snap.ID), slog.String("ref", req.Ref), slog.String("from", req.From), slog.String("to", req.To))
	}
	w.Header().Set("Location", "/v1/job/"+snap.ID)
	code := http.StatusAccepted
	if !created {
		code = http.StatusOK // coalesced onto an identical in-flight move
	}
	writeJSON(w, code, createJobResponse{JobSnapshot: snap, Coalesced: !created})
}

// createJobResponse is the POST /v1/job body: the job snapshot plus whether the
// submission coalesced onto an existing identical move (200) instead of
// creating a new job (202).
type createJobResponse struct {
	warm.JobSnapshot
	Coalesced bool `json:"coalesced"`
}

// handleListJobs godoc
//
//	@Summary	List jobs
//	@Tags		jobs
//	@Produce	json
//	@Param		state	query		string	false	"filter by state"	Enums(pending,running,done,failed,canceled)
//	@Param		ref		query		string	false	"filter by ref substring"
//	@Param		since	query		string	false	"only jobs created at/after this RFC 3339 instant"
//	@Param		limit	query		integer	false	"return at most this many jobs (newest first)"
//	@Success	200		{object}	jobListResponse
//	@Failure	400		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/job [get]
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := warm.Filter{
		State: warm.JobState(q.Get("state")),
		Ref:   q.Get("ref"),
	}
	if f.State != "" && !f.State.Valid() {
		// A typo like ?state=complete would otherwise silently return [] —
		// indistinguishable from "no jobs", which is poison for automation.
		writeErr(w, http.StatusBadRequest, "unknown state "+string(f.State))
		return
	}
	if v := q.Get("since"); v != "" {
		at, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid since: "+err.Error())
			return
		}
		f.Since = at
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit: "+v)
			return
		}
		f.Limit = n
	}
	writeJSON(w, http.StatusOK, jobListResponse{Items: s.store.List(f)})
}

// handleRetryJob godoc
//
//	@Summary	Retry a terminal job
//	@Description	Re-submits the job's ORIGINAL request: fresh store resolution, fresh signature verification, fresh digest pin — a mutable tag is re-verified, never replayed from the stale plan. The retry coalesces/dedups like any submission.
//	@Tags		jobs
//	@Produce	json
//	@Param		id	path		string	true	"job id"
//	@Success	202	{object}	createJobResponse
//	@Header		202	{string}	Location	"canonical URL of the new job"
//	@Failure	404	{object}	errorResponse
//	@Failure	409	{object}	errorResponse	"job has not reached a terminal state"
//	@Failure	422	{object}	errorResponse	"source image signature verification failed"
//	@Failure	503	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/job/{id}/retry [post]
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	snap, created, err := s.warmer.Retry(r.PathValue("id"))
	switch {
	case errors.Is(err, warm.ErrJobNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, warm.ErrJobActive):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, warm.ErrQueueFull):
		writeErr(w, http.StatusServiceUnavailable, "job queue is full")
		return
	case errors.Is(err, verify.ErrUnsigned), errors.Is(err, verify.ErrUntrusted):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Location", "/v1/job/"+snap.ID)
	code := http.StatusAccepted
	if !created {
		code = http.StatusOK
	}
	writeJSON(w, code, createJobResponse{JobSnapshot: snap, Coalesced: !created})
}

// handleCancelJob godoc
//
//	@Summary	Cancel a job, keeping its record
//	@Description	Cancels the job's execution but keeps the record, so the terminal canceled state — which layers finished, how many bytes moved — stays inspectable. DELETE remains the evict operation. A resubmit after cancel starts a fresh job (no coalescing onto the dying one).
//	@Tags		jobs
//	@Produce	json
//	@Param		id	path		string	true	"job id"
//	@Success	202	{object}	warm.JobSnapshot
//	@Failure	404	{object}	errorResponse
//	@Failure	409	{object}	errorResponse	"job already terminal"
//	@Security	BearerAuth
//	@Router		/v1/job/{id}/cancel [post]
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.store.Job(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	snap, _ := s.store.Snapshot(id)
	if snap.State.Terminal() {
		writeErr(w, http.StatusConflict, "job is already "+string(snap.State))
		return
	}
	// Cancel is race-free outside the store lock: the cancel func is set once
	// at submit time before the job is added.
	job.Cancel()
	snap, _ = s.store.Snapshot(id)
	writeJSON(w, http.StatusAccepted, snap)
}

// handlePlanJob godoc
//
//	@Summary	Dry-run a job admission
//	@Description	Resolves the same plan POST /v1/job would — store bindings, the rewritten cache ref, engine pull refs, signature verification with the pinned digest, referrer propagation — without moving bytes or creating a job. `coalesces` names the active job an identical submission would join.
//	@Tags		jobs
//	@Accept		json
//	@Produce	json
//	@Param		request	body		createJobRequest	true	"job request"
//	@Success	200		{object}	warm.PlanResult
//	@Failure	400		{object}	errorResponse
//	@Failure	422		{object}	errorResponse	"source image signature verification failed"
//	@Security	BearerAuth
//	@Router		/v1/job/plan [post]
func (s *Server) handlePlanJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	plan, err := s.warmer.Plan(r.Context(), warm.Request{
		Ref:           req.Ref,
		Platforms:     req.Platforms,
		From:          req.From,
		To:            req.To,
		Distribute:    req.Distribute,
		CopyReferrers: req.CopyReferrers,
	})
	switch {
	case errors.Is(err, verify.ErrUnsigned), errors.Is(err, verify.ErrUntrusted):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
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
//	@Summary	Evict a job record
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
