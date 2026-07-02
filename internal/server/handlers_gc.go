package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/otx/log"
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
		_, _ = s.gc.Index().Delete(name, req.Ref)
	}
	s.rec.ImageRemoved(name, req.Ref)
	log.From(r.Context()).Info("image removed",
		slog.String("store", name), slog.String("ref", req.Ref),
		slog.Any("untagged", res.Untagged), slog.Any("deleted", res.Deleted))
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
			for _, pin := range req.Pins {
				if !doublestar.ValidatePattern(pin) {
					writeErr(w, http.StatusBadRequest, "invalid pin pattern "+pin)
					return
				}
			}
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
	Ref     string `json:"ref,omitempty" example:"docker.io/library/nginx:1.27"` // Exact image reference to pin or unpin.
	Pattern string `json:"pattern,omitempty" example:"*:stable"`                 // Doublestar pattern, matched against the full ref, name:tag, and the bare tag.
}

// value returns the single pin value and whether it is a pattern: exactly one
// of ref/pattern must be set.
func (r pinRequest) value() (value string, pattern, ok bool) {
	if (r.Ref == "") == (r.Pattern == "") {
		return "", false, false
	}
	if r.Ref != "" {
		return r.Ref, false, true
	}
	return r.Pattern, true, true
}

// pinListResponse is the GET /v1/store/{name}/pin body.
type pinListResponse struct {
	Pins []retention.PinEntry `json:"pins"`
}

// handleListPins godoc
//
//	@Summary	List pinned references for a store
//	@Description	Pins are exempt from retention GC: exact references, or doublestar patterns matched against the full ref, its name:tag short form, and the bare tag.
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
		pins = []retention.PinEntry{}
	}
	writeJSON(w, http.StatusOK, pinListResponse{Pins: pins})
}

// handlePin godoc
//
//	@Summary	Pin a reference or pattern (exempt from GC)
//	@Description	Body carries exactly one of `ref` (exact) or `pattern` (doublestar; matched against the full ref, name:tag, and the bare tag). Preview the effect with the GC dry-run — a broad pattern like `**` disables age GC entirely.
//	@Tags		retention
//	@Accept		json
//	@Param		name	path	string		true	"engine store name"
//	@Param		request	body	pinRequest	true	"reference or pattern to pin"
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
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	value, pattern, ok := req.value()
	if !ok {
		writeErr(w, http.StatusBadRequest, "exactly one of ref or pattern is required")
		return
	}
	// A malformed pattern would be accepted and then never match — the image the
	// user believes is pinned gets GC'd. Fail closed here instead.
	if pattern && !doublestar.ValidatePattern(value) {
		writeErr(w, http.StatusBadRequest, "invalid doublestar pattern")
		return
	}
	if err := s.gc.Pin(name, value, pattern); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleUnpin godoc
//
//	@Summary	Unpin a reference or pattern
//	@Tags		retention
//	@Accept		json
//	@Param		name	path	string		true	"engine store name"
//	@Param		request	body	pinRequest	true	"reference or pattern to unpin"
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
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	value, _, ok := req.value()
	if !ok {
		writeErr(w, http.StatusBadRequest, "exactly one of ref or pattern is required")
		return
	}
	if err := s.gc.Unpin(name, value); err != nil {
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

// handleGCStatus godoc
//
//	@Summary	GC scheduler status
//	@Description	Scheduler observability: when GC last ran and will next wake, whether the post-startup grace window is holding age deletion off, the effective default policy, and per-engine index sizes. Never probes live daemons.
//	@Tags		retention
//	@Produce	json
//	@Success	200	{object}	retention.Status
//	@Failure	501	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/gc [get]
func (s *Server) handleGCStatus(w http.ResponseWriter, r *http.Request) {
	if s.gc == nil {
		writeErr(w, http.StatusNotImplemented, "retention/gc is not enabled (set serve.retention.path)")
		return
	}
	writeJSON(w, http.StatusOK, s.gc.Status())
}

// handleStoreWatcher godoc
//
//	@Summary	Usage-watcher status for a store
//	@Description	Liveness of the engine's usage-event stream. A dead stream freezes last-used times and silently degrades age GC — alert on connected=false or a stale last_event_at.
//	@Tags		retention
//	@Produce	json
//	@Param		name	path		string	true	"engine store name"
//	@Success	200		{object}	retention.WatcherStatus
//	@Failure	404		{object}	errorResponse
//	@Failure	501		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/watcher [get]
func (s *Server) handleStoreWatcher(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.gcReady(w, name) {
		return
	}
	ws, ok := s.gc.Watcher(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "store has no retention watcher")
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// imageListResponse is the GET /v1/store/{name}/image body.
type imageListResponse struct {
	Items []retention.Record `json:"items"`
}

// handleStoreImages godoc
//
//	@Summary	List a store's image inventory
//	@Description	The retention index records for an engine store — what gantry believes is on the node and the timestamps driving GC decisions (last_used, last_distributed, first_seen, pinned).
//	@Tags		retention
//	@Produce	json
//	@Param		name	path		string	true	"engine store name"
//	@Param		repo	query		string	false	"filter by exact repository"
//	@Param		ref		query		string	false	"return only this exact reference"
//	@Param		pinned	query		boolean	false	"filter by pinned state"
//	@Success	200		{object}	imageListResponse
//	@Failure	400		{object}	errorResponse
//	@Failure	404		{object}	errorResponse
//	@Failure	500		{object}	errorResponse
//	@Failure	501		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/image [get]
func (s *Server) handleStoreImages(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.gcReady(w, name) {
		return
	}
	recs, err := s.gc.Index().List(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	var pinned *bool
	if v := q.Get("pinned"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid pinned filter: "+v)
			return
		}
		pinned = &b
	}
	items := make([]retention.Record, 0, len(recs))
	for _, rec := range recs {
		if repo := q.Get("repo"); repo != "" && rec.Repo != repo {
			continue
		}
		if ref := q.Get("ref"); ref != "" && rec.Ref != ref {
			continue
		}
		if pinned != nil && rec.Pinned != *pinned {
			continue
		}
		items = append(items, rec)
	}
	writeJSON(w, http.StatusOK, imageListResponse{Items: items})
}

// handleStoreImageDelete godoc
//
//	@Summary	Delete a retention index record
//	@Description	Purges one record from the retention index WITHOUT touching the engine — the escape hatch for orphan records left by out-of-band image removal. A record for an image still present is re-created (with fresh timestamps) by the usage watcher or the next distribute. To delete the image itself use /remove.
//	@Tags		retention
//	@Accept		json
//	@Param		name	path	string			true	"engine store name"
//	@Param		request	body	removeRequest	true	"reference whose record to purge"
//	@Success	204
//	@Failure	400	{object}	errorResponse
//	@Failure	404	{object}	errorResponse
//	@Failure	500	{object}	errorResponse
//	@Failure	501	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/image [delete]
func (s *Server) handleStoreImageDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.gcReady(w, name) {
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
	existed, err := s.gc.Index().Delete(name, req.Ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !existed {
		writeErr(w, http.StatusNotFound, "no index record for "+req.Ref)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// eventListResponse is the GET /v1/event body.
type eventListResponse struct {
	Items []event.Event `json:"items"`
}

// handleListEvents godoc
//
//	@Summary	Query the audit log
//	@Description	Operationally significant events (jobs admitted/finished, GC applied, manual pull/remove, pins) that survive process restart — the durable record the in-memory job store cannot provide. Newest first.
//	@Tags		meta
//	@Produce	json
//	@Param		type	query		string	false	"filter by event type"	Enums(job_admitted,job_done,gc_applied,image_pulled,image_removed,pinned,unpinned)
//	@Param		store	query		string	false	"filter by store"
//	@Param		ref		query		string	false	"filter by exact reference"
//	@Param		state	query		string	false	"filter by job terminal state"
//	@Param		since	query		string	false	"only events at/after this RFC 3339 instant"
//	@Param		limit	query		integer	false	"max events (default 100, max 1000)"
//	@Success	200		{object}	eventListResponse
//	@Failure	400		{object}	errorResponse
//	@Failure	501		{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/event [get]
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeErr(w, http.StatusNotImplemented, "the audit log is not enabled (set serve.events.path)")
		return
	}
	q := r.URL.Query()
	f := event.Filter{
		Type:  event.Type(q.Get("type")),
		Store: q.Get("store"),
		Ref:   q.Get("ref"),
		State: q.Get("state"),
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
	items, err := s.events.List(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []event.Event{}
	}
	writeJSON(w, http.StatusOK, eventListResponse{Items: items})
}
