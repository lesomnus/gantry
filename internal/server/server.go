// Package server exposes the job engine over an HTTP/1 JSON API built on the
// stdlib ServeMux. It is pure transport: decode, call the core, encode.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/server/oapi"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/warm"
)

// Server holds the dependencies shared by the handlers.
type Server struct {
	warmer *warm.Warmer
	store  warm.Store
	stores *store.Set
	gc     *retention.Manager // nil when retention/GC is disabled
}

// New builds the API handler. gc may be nil. Wrap it with Auth for authentication.
func New(warmer *warm.Warmer, jobStore warm.Store, stores *store.Set, gc *retention.Manager) http.Handler {
	s := &Server{warmer: warmer, store: jobStore, stores: stores, gc: gc}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/job", s.handleCreateJob)
	mux.HandleFunc("GET /v1/job", s.handleListJobs)
	mux.HandleFunc("GET /v1/job/{id}", s.handleGetJob)
	mux.HandleFunc("DELETE /v1/job/{id}", s.handleDeleteJob)
	mux.HandleFunc("GET /v1/job/{id}/progress", s.handleProgress)
	mux.HandleFunc("GET /v1/store", s.handleListStores)
	mux.HandleFunc("POST /v1/store/{name}/pull", s.handleStorePull)
	mux.HandleFunc("POST /v1/store/{name}/remove", s.handleStoreRemove)
	mux.HandleFunc("GET /v1/store/{name}/gc", s.handleStoreGC)
	mux.HandleFunc("POST /v1/store/{name}/gc", s.handleStoreGC)
	mux.HandleFunc("GET /v1/store/{name}/pin", s.handleListPins)
	mux.HandleFunc("POST /v1/store/{name}/pin", s.handlePin)
	mux.HandleFunc("DELETE /v1/store/{name}/pin", s.handleUnpin)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// The OpenAPI 3.1 schema (public; exempt from auth like /healthz). The server
	// only exposes the contract — point any viewer (Scalar, Redoc, Swagger UI, an
	// IDE, ...) at it.
	mux.HandleFunc("GET /openapi.json", spec("application/json", oapi.JSON))
	mux.HandleFunc("GET /openapi.yaml", spec("application/yaml", oapi.YAML))
	return mux
}

func spec(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write(body)
	}
}

// handleHealthz godoc
//
//	@Summary	Liveness probe
//	@Tags		meta
//	@Produce	plain
//	@Success	200	{string}	string	"ok"
//	@Router		/healthz [get]
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "ok")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorResponse{Error: msg})
}

// errorResponse is the body of every error reply.
type errorResponse struct {
	Error string `json:"error"`
}

// jobListResponse is the GET /v1/job body.
type jobListResponse struct {
	Items []warm.JobSnapshot `json:"items"`
}

// storeListResponse is the GET /v1/store body.
type storeListResponse struct {
	Items []store.Status `json:"items"`
}

// storePullResponse is the POST /v1/store/{name}/pull body.
type storePullResponse struct {
	Store string `json:"store"`
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	State string `json:"state"`
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeSSE(w io.Writer, event string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func duration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
