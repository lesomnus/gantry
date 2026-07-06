package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/lesomnus/gantry/internal/down"
)

type nopSink struct{}

func (nopSink) Layer(down.LayerUpdate) {}

// handleListStores godoc
//
//	@Summary	List stores
//	@Description	Configured stores with their kind, capabilities, and readiness.
//	@Tags		stores
//	@Produce	json
//	@Success	200	{object}	storeListResponse
//	@Security	BearerAuth
//	@Router		/v1/store [get]
func (s *Server) handleListStores(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, storeListResponse{Items: s.stores.StoreStatuses(r.Context())})
}

type storePullRequest struct {
	Ref string `json:"ref" binding:"required" example:"docker.io/library/nginx:1.27"` // Image reference the engine store should pull (required).
	// Platform to pull ("os/arch", e.g. "linux/arm64"); empty uses the daemon's
	// default. Passed through as-is — an unavailable platform is the daemon's error.
	Platform string `json:"platform,omitempty" example:"linux/arm64"`
}

// handleStorePull godoc
//
//	@Summary	Trigger an engine store to pull
//	@Description	Tells one engine store to pull a reference, decoupled from the job pipeline (manual reconcile). Stamps the retention index so the pulled image stays eligible for age GC.
//	@Tags		stores
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string	true	"engine store name"
//	@Param		request	body		storePullRequest	true	"reference to pull"
//	@Success	200		{object}	storePullResponse
//	@Failure	400		{object}	errorResponse
//	@Failure	404		{object}	errorResponse
//	@Failure	502		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/pull [post]
func (s *Server) handleStorePull(w http.ResponseWriter, r *http.Request) {
	eng, err := s.stores.Engine(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var req storePullRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	if err := eng.Pull(r.Context(), req.Ref, "", req.Platform, nopSink{}); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if s.gc != nil {
		// Stamp the retention index: without a record the retention
		// policy can never age-collect a manually pulled image.
		s.gc.Distributed(eng.Name(), req.Ref, time.Now())
	}
	s.rec.ImagePulled(eng.Name(), req.Ref)
	writeJSON(w, http.StatusOK, storePullResponse{
		Store: eng.Name(),
		Kind:  eng.Kind(),
		Ref:   req.Ref,
		State: "done",
	})
}

// inUseResponse is the GET /v1/store/{name}/inuse body.
type inUseResponse struct {
	// InUse mixes tag references and image IDs (sha256:...), exactly as live
	// containers reference them.
	InUse []string `json:"in_use"`
}

// handleStoreInUse godoc
//
//	@Summary	List images live containers hold on an engine
//	@Description	The references and image IDs running containers use right now — GC's top protection tier, as ground truth (probed live, not from the index).
//	@Tags		stores
//	@Produce	json
//	@Param		name	path		string	true	"engine store name"
//	@Success	200		{object}	inUseResponse
//	@Failure	404		{object}	errorResponse
//	@Failure	502		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/inuse [get]
func (s *Server) handleStoreInUse(w http.ResponseWriter, r *http.Request) {
	eng, err := s.stores.Engine(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	m, err := eng.InUse(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	refs := make([]string, 0, len(m))
	for ref := range m {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	writeJSON(w, http.StatusOK, inUseResponse{InUse: refs})
}
