package server

import (
	"net/http"

	"github.com/lesomnus/gantry/internal/down"
)

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	var items []down.TargetStatus
	if s.targets != nil {
		items = s.targets.Status(r.Context())
	} else {
		items = []down.TargetStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type targetPullRequest struct {
	Ref string `json:"ref"`
}

// handleTargetPull triggers a single target to pull a reference, decoupled from
// the warm pipeline (manual reconcile / re-trigger on a new node).
func (s *Server) handleTargetPull(w http.ResponseWriter, r *http.Request) {
	target, ok := s.targets.Get(r.PathValue("name"))
	if !ok {
		writeErr(w, http.StatusNotFound, "target not found")
		return
	}
	var req targetPullRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	if err := target.Pull(r.Context(), req.Ref); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"name":  target.Name(),
		"kind":  target.Kind(),
		"ref":   req.Ref,
		"state": "pulled",
	})
}
