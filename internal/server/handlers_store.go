package server

import (
	"net/http"

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
}

// handleStorePull godoc
//
//	@Summary	Trigger an engine store to pull
//	@Description	Tells one engine store to pull a reference, decoupled from the job pipeline (manual reconcile).
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
	if err := eng.Pull(r.Context(), req.Ref, nopSink{}); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, storePullResponse{
		Store: eng.Name(),
		Kind:  eng.Kind(),
		Ref:   req.Ref,
		State: "done",
	})
}
