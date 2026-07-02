package server

import (
	"errors"
	"net/http"
	"sync"

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

// readyzResponse is the GET /readyz body. Stores is included only for
// authenticated callers — probe error strings carry internal hosts and socket
// addresses, which an anonymous prober has no business reading.
type readyzResponse struct {
	Ready  bool            `json:"ready"`
	Stores []health.Report `json:"stores,omitempty"`
}

// handleReadyz godoc
//
//	@Summary	Readiness probe
//	@Description	Aggregate readiness over the gated stores (serve.health.ready_stores; defaults to every engine store, so a flaky remote upstream cannot flap the node). 200 when all gated stores are healthy, 503 otherwise. Unauthenticated like /healthz, but the per-store breakdown is included only for authenticated callers.
//	@Tags		meta
//	@Produce	json
//	@Success	200	{object}	readyzResponse
//	@Failure	503	{object}	readyzResponse
//	@Router		/readyz [get]
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	gate := s.health.ReadyStores()
	if len(gate) == 0 {
		gate = s.stores.EngineNames()
	}
	reports := make([]health.Report, len(gate))
	var wg sync.WaitGroup
	for i, name := range gate {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			rep, err := s.health.Check(r.Context(), name)
			if err != nil {
				rep = health.Report{Name: name, Healthy: false, Error: err.Error()}
			}
			reports[i] = rep
		}(i, name)
	}
	wg.Wait()
	ready := true
	for _, rep := range reports {
		if !rep.Healthy {
			ready = false
			break
		}
	}
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	res := readyzResponse{Ready: ready}
	if isAuthed(r.Context()) {
		res.Stores = reports
	}
	writeJSON(w, code, res)
}
