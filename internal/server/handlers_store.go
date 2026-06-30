package server

import (
	"net/http"

	"github.com/lesomnus/gantry/internal/down"
)

type nopSink struct{}

func (nopSink) Layer(down.LayerUpdate) {}

func (s *Server) handleListStores(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": s.stores.StoreStatuses(r.Context())})
}

type storePullRequest struct {
	Ref string `json:"ref"`
}

// handleStorePull triggers one engine store to pull a reference, decoupled from
// the job pipeline (manual reconcile / re-trigger on a new node).
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
	writeJSON(w, http.StatusOK, map[string]string{
		"store": eng.Name(),
		"kind":  eng.Kind(),
		"ref":   req.Ref,
		"state": "done",
	})
}
