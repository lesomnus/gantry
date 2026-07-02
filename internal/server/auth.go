package server

import (
	"context"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/lesomnus/gantry/cmd/config"
)

// authedKey marks a request that passed authentication (or runs with auth
// disabled), letting public endpoints add detail for operators only.
type authedKey struct{}

func isAuthed(ctx context.Context) bool {
	v, _ := ctx.Value(authedKey{}).(bool)
	return v
}

// Auth guards /v1/* with a bearer-token whitelist and/or mTLS. /healthz is
// always exempt. If neither tokens nor a client CA are configured, auth is
// disabled (suitable behind a trusted reverse proxy). Token values are
// environment-expanded (e.g. "${GANTRY_TOKEN}").
func Auth(cfg config.AuthConfig) func(http.Handler) http.Handler {
	tokens := make([][]byte, 0, len(cfg.Tokens))
	for _, t := range cfg.Tokens {
		if v := os.ExpandEnv(t); v != "" {
			tokens = append(tokens, []byte(v))
		}
	}
	mtls := cfg.ClientCA != ""
	enabled := len(tokens) > 0 || mtls

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authed := !enabled || authorized(r, tokens, mtls)
			if authed {
				r = r.WithContext(context.WithValue(r.Context(), authedKey{}, true))
			}
			if authed || isPublic(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "unauthorized")
		})
	}
}

// isPublic reports whether a path bypasses auth (probes + the API schema).
func isPublic(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/openapi.json" || path == "/openapi.yaml"
}

func authorized(r *http.Request, tokens [][]byte, mtls bool) bool {
	if mtls && r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		return true
	}
	tok := bearer(r)
	if tok == nil {
		return false
	}
	for _, want := range tokens {
		if subtle.ConstantTimeCompare(tok, want) == 1 {
			return true
		}
	}
	return false
}

func bearer(r *http.Request) []byte {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return nil
	}
	return []byte(strings.TrimSpace(h[len(prefix):]))
}
