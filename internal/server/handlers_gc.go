package server

import (
	"net/http"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
)

// handleStoreRemove godoc
//
//	@Summary	Remove an image from an engine store
//	@Description	Manually deletes one image from an engine store and syncs the retention index.
//	@Tags		retention
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string			true	"engine store name"
//	@Param		request	body		removeRequest	true	"reference to remove"
//	@Success	200		{object}	down.RemoveResult
//	@Failure	400		{object}	errorResponse
//	@Failure	404		{object}	errorResponse
//	@Failure	502		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/remove [post]
func (s *Server) handleStoreRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	eng, err := s.stores.Engine(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	var req removeRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	res, err := eng.Remove(r.Context(), req.Ref)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if s.gc != nil {
		_ = s.gc.Index().Delete(name, req.Ref)
	}
	writeJSON(w, http.StatusOK, res)
}

// removeRequest is the POST /v1/store/{name}/remove body.
type removeRequest struct {
	Ref string `json:"ref" binding:"required" example:"docker.io/library/nginx:1.27"` // Image reference to delete from the engine store (required).
}

// gcRequest overrides the configured policy for one /gc call. Fields are
// pointers so an explicit zero (e.g. keep_n:0 to force keep-N off, or
// max_age:"0s" to disable age GC) is distinguishable from "not set".
type gcRequest struct {
	MaxAge *config.Duration `json:"max_age,omitempty" swaggertype:"string" example:"720h"` // Override max image age (Go duration, e.g. "720h"); "0s" disables age GC.
	KeepN  *int             `json:"keep_n,omitempty" example:"3"`                          // Override how many most-recent tags to keep per repo; 0 disables keep-N.
	Pins   []string         `json:"pins,omitempty" example:"docker.io/library/nginx:1.27"` // Override the pinned references exempt from GC.
}

// handleStoreGCPlan godoc
//
//	@Summary	Evaluate retention GC for a store (dry-run)
//	@Description	Returns the retention decision (keep/delete) without deleting anything. An optional body overrides the configured max_age/keep_n/pins for this call. NOTE: a GET request body is dropped by fetch()/XHR, some HTTP clients, and proxies — to pass overrides reliably, use POST.
//	@Tags		retention
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string		true	"engine store name"
//	@Param		request	body		gcRequest	false	"policy overrides"
//	@Success	200		{object}	retention.Decision	"dry-run keep/delete decision"
//	@Failure	400		{object}	errorResponse
//	@Failure	404		{object}	errorResponse
//	@Failure	501		{object}	errorResponse
//	@Failure	502		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/gc [get]
func (s *Server) handleStoreGCPlan(w http.ResponseWriter, r *http.Request) { s.handleStoreGC(w, r) }

// handleStoreGCApply godoc
//
//	@Summary	Run retention GC for a store (apply)
//	@Description	Applies the deletions and returns the apply result. An optional body overrides the configured max_age/keep_n/pins for this call.
//	@Tags		retention
//	@Accept		json
//	@Produce	json
//	@Param		name	path		string		true	"engine store name"
//	@Param		request	body		gcRequest	false	"policy overrides"
//	@Success	200		{object}	retention.ApplyResult	"deletions applied"
//	@Failure	400		{object}	errorResponse
//	@Failure	404		{object}	errorResponse
//	@Failure	501		{object}	errorResponse
//	@Failure	502		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/gc [post]
func (s *Server) handleStoreGCApply(w http.ResponseWriter, r *http.Request) { s.handleStoreGC(w, r) }

// handleStoreGC is the shared GET(dry-run)/POST(apply) logic behind the two
// documented wrappers above.
func (s *Server) handleStoreGC(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.stores.Engine(name); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if s.gc == nil {
		writeErr(w, http.StatusNotImplemented, "retention/gc is not enabled (set serve.retention.path)")
		return
	}
	p := s.gc.DefaultPolicy()
	if r.ContentLength != 0 {
		var req gcRequest
		if err := readJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.MaxAge != nil {
			p.MaxAge = time.Duration(*req.MaxAge)
		}
		if req.KeepN != nil {
			p.KeepN = *req.KeepN
		}
		if req.Pins != nil {
			p.Pins = req.Pins
		}
	}
	dec, err := s.gc.Plan(r.Context(), name, p)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, dec)
		return
	}
	res, err := s.gc.Apply(r.Context(), name, dec)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type pinRequest struct {
	Ref string `json:"ref" binding:"required" example:"docker.io/library/nginx:1.27"` // Image reference to pin or unpin (exact-match, required).
}

// pinListResponse is the GET /v1/store/{name}/pin body.
type pinListResponse struct {
	Pins []string `json:"pins"`
}

// handleListPins godoc
//
//	@Summary	List pinned references for a store
//	@Description	Pinned references are exempt from retention GC (exact-match).
//	@Tags		retention
//	@Produce	json
//	@Param		name	path		string	true	"engine store name"
//	@Success	200		{object}	pinListResponse
//	@Failure	404		{object}	errorResponse
//	@Failure	500		{object}	errorResponse
//	@Failure	501		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/pin [get]
func (s *Server) handleListPins(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.gcReady(w, name) {
		return
	}
	pins, err := s.gc.Pins(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pins == nil {
		pins = []string{}
	}
	writeJSON(w, http.StatusOK, pinListResponse{Pins: pins})
}

// handlePin godoc
//
//	@Summary	Pin a reference (exempt from GC)
//	@Tags		retention
//	@Accept		json
//	@Param		name	path	string		true	"engine store name"
//	@Param		request	body	pinRequest	true	"reference to pin"
//	@Success	204
//	@Failure	400	{object}	errorResponse
//	@Failure	404	{object}	errorResponse
//	@Failure	500	{object}	errorResponse
//	@Failure	501	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/pin [post]
func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.gcReady(w, name) {
		return
	}
	var req pinRequest
	if err := readJSON(r, &req); err != nil || req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	if err := s.gc.Pin(name, req.Ref); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUnpin godoc
//
//	@Summary	Unpin a reference
//	@Tags		retention
//	@Accept		json
//	@Param		name	path	string		true	"engine store name"
//	@Param		request	body	pinRequest	true	"reference to unpin"
//	@Success	204
//	@Failure	400	{object}	errorResponse
//	@Failure	404	{object}	errorResponse
//	@Failure	500	{object}	errorResponse
//	@Failure	501	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/pin [delete]
func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.gcReady(w, name) {
		return
	}
	var req pinRequest
	if err := readJSON(r, &req); err != nil || req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	if err := s.gc.Unpin(name, req.Ref); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// gcReady validates the store exists as an engine and retention is enabled.
func (s *Server) gcReady(w http.ResponseWriter, name string) bool {
	if _, err := s.stores.Engine(name); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return false
	}
	if s.gc == nil {
		writeErr(w, http.StatusNotImplemented, "retention/gc is not enabled (set serve.retention.path)")
		return false
	}
	return true
}
