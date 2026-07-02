package server

import (
	"errors"
	"net/http"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/verify"
)

// verifyRequest is the POST /v1/verify body.
type verifyRequest struct {
	Ref  string `json:"ref" binding:"required" example:"docker.io/library/nginx:1.27"` // Image reference to verify (required).
	From string `json:"from" example:"dockerhub"`                                      // Source registry store name or host; defaults to the ref's registry.
}

// verifyResponse is the POST /v1/verify body on success.
type verifyResponse struct {
	Ref      string `json:"ref"`                                        // the resolved source reference that was checked
	Mode     string `json:"mode" enums:"off,verify-if-present,require"` // effective mode for the source store
	Verified bool   `json:"verified"`                                   // a signature was verified
	Digest   string `json:"digest,omitempty"`                           // the verified digest
}

// verifyDescribeResponse wraps the trust configuration with per-store modes.
type verifyDescribeResponse struct {
	verify.Description
	// Stores maps each source registry store to its effective mode (per-store
	// override or the global default).
	Stores map[string]string `json:"stores,omitempty"`
}

// srcRef resolves the request's source store and the reference on it, the same
// way job admission does.
func (s *Server) srcRef(ref, from string) (config.StoreConfig, name.Reference, error) {
	base, err := name.ParseReference(ref)
	if err != nil {
		return config.StoreConfig{}, nil, err
	}
	from_key := from
	if from_key == "" {
		from_key = base.Context().RegistryStr()
	}
	from_store, err := s.stores.Registry(from_key)
	if err != nil {
		return config.StoreConfig{}, nil, err
	}
	id := ":" + base.Identifier()
	if d, ok := base.(name.Digest); ok {
		id = "@" + d.DigestStr()
	}
	src, err := name.ParseReference(from_store.Host + "/" + base.Context().RepositoryStr() + id)
	if err != nil {
		return config.StoreConfig{}, nil, err
	}
	return from_store, src, nil
}

// handleVerifyPreflight godoc
//
//	@Summary	Verify a reference without moving it
//	@Description	Runs the same notation verification job admission uses — resolve the tag on its source store, check the signature against the trust policy — without moving bytes or creating a job. Lets CI gate a rollout on "this gantry will accept the image".
//	@Tags		verify
//	@Accept		json
//	@Produce	json
//	@Param		request	body		verifyRequest	true	"reference to verify"
//	@Success	200		{object}	verifyResponse
//	@Failure	400		{object}	errorResponse
//	@Failure	404		{object}	errorResponse	"reference not found on the source"
//	@Failure	422		{object}	errorResponse	"unsigned, or signature verification failed"
//	@Failure	501		{object}	errorResponse
//	@Failure	502		{object}	errorResponse	"verification could not be completed"
//	@Security	BearerAuth
//	@Router		/v1/verify [post]
func (s *Server) handleVerifyPreflight(w http.ResponseWriter, r *http.Request) {
	if s.verify == nil {
		writeErr(w, http.StatusNotImplemented, "verification is not enabled (set serve.verify.mode)")
		return
	}
	var req verifyRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, "ref is required")
		return
	}
	from, src, err := s.srcRef(req.Ref, req.From)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.verify.Verify(r.Context(), from, src)
	switch {
	case errors.Is(err, verify.ErrUnsigned), errors.Is(err, verify.ErrUntrusted):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, verify.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := verifyResponse{Ref: src.Name(), Mode: string(res.Mode), Verified: res.Verified()}
	if res.Verified() {
		out.Digest = res.Digest.String()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVerifyDescribe godoc
//
//	@Summary	Effective verification configuration
//	@Description	The trust configuration in effect: provider, global and per-store modes, trust-policy scopes and identities, and each trust anchor's subject, fingerprint, and expiry (never key material). Anchor expiry is an outage in require mode — alert on not_after.
//	@Tags		verify
//	@Produce	json
//	@Success	200	{object}	verifyDescribeResponse
//	@Security	BearerAuth
//	@Router		/v1/verify [get]
func (s *Server) handleVerifyDescribe(w http.ResponseWriter, r *http.Request) {
	if s.verify == nil {
		writeJSON(w, http.StatusOK, verifyDescribeResponse{
			Description: verify.Description{Enabled: false, Mode: string(config.VerifyOff)},
		})
		return
	}
	out := verifyDescribeResponse{Description: s.verify.Describe(), Stores: map[string]string{}}
	global := config.VerifyMode(out.Mode)
	for _, n := range s.stores.Names() {
		c, ok := s.stores.Config(n)
		if !ok || !c.IsRegistry() {
			continue
		}
		mode := global
		if c.Verify != nil && c.Verify.Mode != "" {
			mode = c.Verify.Mode
		}
		if mode == "" {
			mode = config.VerifyOff
		}
		out.Stores[n] = string(mode)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVerifyReload godoc
//
//	@Summary	Reload the trust store and policy
//	@Description	Re-reads the CA directory and trust policy from disk with the same fail-fast validation as startup, atomically swapping the verifier on success. On failure the previous verifier stays active. Makes CA/leaf rotation a fleet-safe operation without restarting gantry.
//	@Tags		verify
//	@Produce	json
//	@Success	200	{object}	verify.Description	"the newly loaded configuration"
//	@Failure	422	{object}	errorResponse		"new trust material rejected; old verifier retained"
//	@Failure	500	{object}	errorResponse		"reload could not complete (transient); old verifier retained"
//	@Failure	501	{object}	errorResponse
//	@Security	BearerAuth
//	@Router		/v1/verify/reload [post]
func (s *Server) handleVerifyReload(w http.ResponseWriter, r *http.Request) {
	if s.verify == nil {
		writeErr(w, http.StatusNotImplemented, "verification is not enabled (set serve.verify.mode)")
		return
	}
	desc, err := s.verify.Reload()
	switch {
	case errors.Is(err, verify.ErrBadTrustMaterial):
		writeErr(w, http.StatusUnprocessableEntity, "reload rejected: "+err.Error())
		return
	case err != nil:
		// Transient (I/O mid-rotation, etc.): the material may be fine — retry.
		writeErr(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, desc)
}
