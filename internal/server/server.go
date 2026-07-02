// Package server exposes the job engine over an HTTP/1 JSON API built on the
// stdlib ServeMux. It is pure transport: decode, call the core, encode.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/lesomnus/gantry/cmd/version"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/server/oapi"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
)

// Server holds the dependencies shared by the handlers.
type Server struct {
	warmer *warm.Warmer
	store  warm.Store
	stores *store.Set
	gc     *retention.Manager // nil when retention/GC is disabled
	health *health.Checker
	verify verify.Service // nil when verification is disabled
}

// New builds the API handler. gc and vf may be nil. Wrap it with Auth for
// authentication.
func New(warmer *warm.Warmer, jobStore warm.Store, stores *store.Set, gc *retention.Manager, hc *health.Checker, vf verify.Service) http.Handler {
	// A typed-nil Service (a nil *verify.Swappable in the interface) must mean
	// disabled; otherwise every nil-guard passes and handlers dereference it.
	if v := reflect.ValueOf(vf); vf != nil && v.Kind() == reflect.Pointer && v.IsNil() {
		vf = nil
	}
	s := &Server{warmer: warmer, store: jobStore, stores: stores, gc: gc, health: hc, verify: vf}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/job", s.handleCreateJob)
	mux.HandleFunc("POST /v1/job/plan", s.handlePlanJob)
	mux.HandleFunc("GET /v1/job", s.handleListJobs)
	mux.HandleFunc("POST /v1/job/{id}/retry", s.handleRetryJob)
	mux.HandleFunc("POST /v1/job/{id}/cancel", s.handleCancelJob)
	mux.HandleFunc("GET /v1/job/{id}", s.handleGetJob)
	mux.HandleFunc("DELETE /v1/job/{id}", s.handleDeleteJob)
	mux.HandleFunc("GET /v1/job/{id}/progress", s.handleProgress)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("GET /v1/verify", s.handleVerifyDescribe)
	mux.HandleFunc("POST /v1/verify", s.handleVerifyPreflight)
	mux.HandleFunc("POST /v1/verify/reload", s.handleVerifyReload)
	mux.HandleFunc("GET /v1/gc", s.handleGCStatus)
	mux.HandleFunc("GET /v1/store", s.handleListStores)
	mux.HandleFunc("GET /v1/store/{name}/health", s.handleStoreHealth)
	mux.HandleFunc("POST /v1/store/{name}/pull", s.handleStorePull)
	mux.HandleFunc("POST /v1/store/{name}/remove", s.handleStoreRemove)
	mux.HandleFunc("GET /v1/store/{name}/gc", s.handleStoreGCPlan)
	mux.HandleFunc("POST /v1/store/{name}/gc", s.handleStoreGCApply)
	mux.HandleFunc("GET /v1/store/{name}/inuse", s.handleStoreInUse)
	mux.HandleFunc("GET /v1/store/{name}/watcher", s.handleStoreWatcher)
	mux.HandleFunc("GET /v1/store/{name}/image", s.handleStoreImages)
	mux.HandleFunc("DELETE /v1/store/{name}/image", s.handleStoreImageDelete)
	mux.HandleFunc("GET /v1/store/{name}/pin", s.handleListPins)
	mux.HandleFunc("POST /v1/store/{name}/pin", s.handlePin)
	mux.HandleFunc("DELETE /v1/store/{name}/pin", s.handleUnpin)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

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

// versionResponse is the GET /v1/version body.
type versionResponse struct {
	Version  string `json:"version"`
	GitRev   string `json:"git_rev"`
	GitDirty bool   `json:"git_dirty"`
}

// handleVersion godoc
//
//	@Summary	Build info
//	@Description	The running binary's version stamp, for remote fleet inventory during rolling upgrades.
//	@Tags		meta
//	@Produce	json
//	@Success	200	{object}	versionResponse
//	@Security	BearerAuth
//	@Router		/v1/version [get]
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	b := version.Get()
	writeJSON(w, http.StatusOK, versionResponse{Version: b.Version, GitRev: b.GitRev, GitDirty: b.GitDirty})
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
