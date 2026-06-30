// Package server exposes the warm engine over an HTTP/1 JSON API built on the
// stdlib ServeMux (method + path patterns). It is pure transport: decode, call
// the core, encode.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/warm"
)

// Server holds the dependencies shared by the handlers.
type Server struct {
	warmer  *warm.Warmer
	store   warm.Store
	targets *down.Registry
}

// New builds the API handler. targets may be nil. Wrap the result with Auth for
// authentication.
func New(warmer *warm.Warmer, store warm.Store, targets *down.Registry) http.Handler {
	s := &Server{warmer: warmer, store: store, targets: targets}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/job", s.handleCreateWarm)
	mux.HandleFunc("GET /v1/job", s.handleListWarms)
	mux.HandleFunc("GET /v1/job/{id}", s.handleGetWarm)
	mux.HandleFunc("DELETE /v1/job/{id}", s.handleDeleteWarm)
	mux.HandleFunc("GET /v1/job/{id}/progress", s.handleProgress)
	mux.HandleFunc("GET /v1/target", s.handleListTargets)
	mux.HandleFunc("POST /v1/target/{name}/pull", s.handleTargetPull)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

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
	writeJSON(w, code, map[string]string{"error": msg})
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

// duration parses a positive duration query value, returning ok=false if absent
// or invalid.
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
