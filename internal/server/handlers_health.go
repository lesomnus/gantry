package server

import (
	"errors"
	"net/http"

	"github.com/lesomnus/gantry/internal/health"
)

// handleStoreHealth godoc
//
//	@Summary	Check a store's health
//	@Description	Probes one store's reachability (an engine daemon ready-check, or a registry GET /v2/ ping) and returns the result. The probe is cached for a short TTL (serve.health.cache_ttl, default 5s); a cached response sets `cached: true`. Returns 200 when healthy, 503 when unhealthy (report body either way), 404 for an unknown store.
//	@Tags		stores
//	@Produce	json
//	@Param		name	path		string	true	"store name"
//	@Success	200		{object}	health.Report
//	@Failure	404		{object}	errorResponse
//	@Failure	503		{object}	health.Report
//	@Security	BearerAuth
//	@Router		/v1/store/{name}/health [get]
func (s *Server) handleStoreHealth(w http.ResponseWriter, r *http.Request) {
	rep, err := s.health.Check(r.Context(), r.PathValue("name"))
	if err != nil {
		if errors.Is(err, health.ErrUnknownStore) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	code := http.StatusOK
	if !rep.Healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, rep)
}
