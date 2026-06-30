package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/gantry/cmd/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestAuthDisabledWhenUnconfigured(t *testing.T) {
	h := Auth(config.AuthConfig{})(okHandler())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/job", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d, want 200 (auth off)", rr.Code)
	}
}

func TestAuthBearer(t *testing.T) {
	h := Auth(config.AuthConfig{Tokens: []string{"s3cret"}})(okHandler())
	t.Run("no token is rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/job", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("code = %d, want 401", rr.Code)
		}
	})
	t.Run("wrong token is rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/job", nil)
		req.Header.Set("Authorization", "Bearer nope")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("code = %d, want 401", rr.Code)
		}
	})
	t.Run("correct token passes", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/job", nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("code = %d, want 200", rr.Code)
		}
	})
	t.Run("healthz is exempt", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
		if rr.Code != http.StatusOK {
			t.Errorf("code = %d, want 200", rr.Code)
		}
	})
	t.Run("openapi schema is exempt", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/openapi.json", nil))
		if rr.Code != http.StatusOK {
			t.Errorf("code = %d, want 200", rr.Code)
		}
	})
}

func TestAuthEnvExpansion(t *testing.T) {
	t.Setenv("GANTRY_TOKEN", "from-env")
	h := Auth(config.AuthConfig{Tokens: []string{"${GANTRY_TOKEN}"}})(okHandler())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/job", nil)
	req.Header.Set("Authorization", "Bearer from-env")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rr.Code)
	}
}
