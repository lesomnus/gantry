package verify

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/notaryproject/notation-go"
	notationregistry "github.com/notaryproject/notation-go/registry"
	"github.com/notaryproject/notation-go/verifier"
	"github.com/notaryproject/notation-go/verifier/trustpolicy"
	"github.com/notaryproject/notation-go/verifier/truststore"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// notaryVerifier verifies OCI-native (Notary Project / notation) signatures.
type notaryVerifier struct {
	cfg config.VerifyConfig
	v   notation.Verifier
}

// New builds a notation Verifier from the config. It fails fast (loads and
// validates the trust store + policy now) so a misconfiguration stops startup
// rather than failing every job later. Call only when cfg.Enabled().
func New(cfg config.VerifyConfig) (Verifier, error) {
	if cfg.Provider != "" && cfg.Provider != "notation" {
		return nil, fmt.Errorf("verify provider %q is not supported (only \"notation\")", cfg.Provider)
	}
	ts, err := loadTrustStore(cfg)
	if err != nil {
		return nil, err
	}
	doc, err := loadOrSynthPolicy(cfg)
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("trust policy: %w", err)
	}
	nv, err := verifier.New(doc, ts, nil)
	if err != nil {
		return nil, fmt.Errorf("build notation verifier: %w", err)
	}
	return &notaryVerifier{cfg: cfg, v: nv}, nil
}

func (n *notaryVerifier) Verify(ctx context.Context, from config.StoreConfig, src name.Reference) (v1.Hash, error) {
	mode := n.cfg.EffectiveMode(from)
	if mode == config.VerifyOff {
		return v1.Hash{}, nil
	}
	if d := time.Duration(n.cfg.Timeout); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	repo, err := n.repo(from, src)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("build repository client: %w", err)
	}
	// Resolve the tag (or digest) to its manifest descriptor: this is what gets
	// verified and pinned.
	desc, err := repo.Resolve(ctx, src.Identifier())
	if err != nil {
		return v1.Hash{}, fmt.Errorf("resolve %s: %w", src.Name(), err)
	}

	// Gate on signature presence (uniformly for both modes) so we can report a
	// missing signature distinctly (ErrUnsigned) from a bad one (ErrUntrusted).
	signed, err := hasSignature(ctx, repo, desc)
	if err != nil {
		return v1.Hash{}, fmt.Errorf("list signatures for %s: %w", src.Name(), err)
	}
	if !signed {
		if mode == config.VerifyRequire {
			return v1.Hash{}, fmt.Errorf("%w: %s", ErrUnsigned, src.Name())
		}
		return v1.Hash{}, nil // verify-if-present: unsigned is allowed
	}

	artifactRef := src.Context().Digest(desc.Digest.String()).Name()
	if _, _, err := notation.Verify(ctx, n.v, repo, notation.VerifyOptions{
		ArtifactReference:    artifactRef,
		MaxSignatureAttempts: 50,
	}); err != nil {
		return v1.Hash{}, fmt.Errorf("%w: %v", ErrUntrusted, err)
	}

	h, err := v1.NewHash(desc.Digest.String())
	if err != nil {
		return v1.Hash{}, err
	}
	return h, nil
}

// repo builds a notation repository over the source registry, matching gantry's
// auth (basic when credentials are set, else anonymous) and insecure (plain
// HTTP) handling for that store.
func (n *notaryVerifier) repo(from config.StoreConfig, ref name.Reference) (notationregistry.Repository, error) {
	r, err := remote.NewRepository(ref.Context().Name())
	if err != nil {
		return nil, err
	}
	client := &auth.Client{Cache: auth.NewCache()}
	if from.Username != "" {
		client.Credential = auth.StaticCredential(ref.Context().RegistryStr(),
			auth.Credential{Username: from.Username, Password: from.Password})
	}
	r.Client = client
	if from.Insecure {
		r.PlainHTTP = true
	}
	return notationregistry.NewRepository(r), nil
}

// errSignatureFound stops ListSignatures early once any signature is seen.
var errSignatureFound = errors.New("signature found")

func hasSignature(ctx context.Context, repo notationregistry.Repository, desc ocispec.Descriptor) (bool, error) {
	err := repo.ListSignatures(ctx, desc, func(sigs []ocispec.Descriptor) error {
		if len(sigs) > 0 {
			return errSignatureFound
		}
		return nil
	})
	if errors.Is(err, errSignatureFound) {
		return true, nil
	}
	return false, err
}

// --- trust store & policy ---

// certStore is an X509TrustStore backed by a fixed set of CA certificates loaded
// from a directory; it returns them for any named store the policy references.
type certStore struct{ certs []*x509.Certificate }

func (s *certStore) GetCertificates(_ context.Context, storeType truststore.Type, _ string) ([]*x509.Certificate, error) {
	// gantry's flat trust_store is a single CA bundle; only "ca:" stores resolve.
	// Any other type (e.g. signingAuthority) gets no certs -> verification fails
	// closed rather than silently trusting the CA bundle for it.
	if storeType != truststore.TypeCA {
		return nil, nil
	}
	return s.certs, nil
}

func loadTrustStore(cfg config.VerifyConfig) (truststore.X509TrustStore, error) {
	switch cfg.TrustStore {
	case "":
		return nil, errors.New("serve.verify.trust_store is required when verification is enabled")
	case "system":
		return nil, errors.New(`serve.verify.trust_store "system" (OS roots) is not supported; provide a CA certificate directory`)
	}
	certs, err := loadCertDir(cfg.TrustStore)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no CA certificates found in serve.verify.trust_store %q", cfg.TrustStore)
	}
	return &certStore{certs: certs}, nil
}

func loadCertDir(dir string) ([]*x509.Certificate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read trust_store dir: %w", err)
	}
	var certs []*x509.Certificate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".crt", ".pem", ".cert", ".cer":
		default:
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for len(b) > 0 {
			var blk *pem.Block
			blk, b = pem.Decode(b)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				continue
			}
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			certs = append(certs, c)
		}
	}
	return certs, nil
}

// checkSingleCAStore rejects a user-supplied trust policy that references trust
// stores gantry's single flat CA bundle cannot distinguish: any non-"ca:" store,
// or more than one distinct store name. Without this, every referenced store
// resolves to the same bundle, silently over-trusting relative to the operator's
// intent (a teamB-CA signature would satisfy a teamA-scoped policy).
func checkSingleCAStore(doc *trustpolicy.Document) error {
	names := map[string]struct{}{}
	for _, p := range doc.TrustPolicies {
		for _, ts := range p.TrustStores {
			if !strings.HasPrefix(ts, "ca:") {
				return fmt.Errorf("trust_policy references unsupported trust store %q; gantry supports a single ca:<name> store", ts)
			}
			names[ts] = struct{}{}
		}
	}
	if len(names) > 1 {
		return fmt.Errorf("trust_policy references %d distinct trust stores; gantry's trust_store is a single CA bundle — use one ca:<name>", len(names))
	}
	return nil
}

func loadOrSynthPolicy(cfg config.VerifyConfig) (*trustpolicy.Document, error) {
	if cfg.TrustPolicy != "" {
		b, err := os.ReadFile(cfg.TrustPolicy)
		if err != nil {
			return nil, fmt.Errorf("read trust_policy: %w", err)
		}
		var doc trustpolicy.Document
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("parse trust_policy: %w", err)
		}
		if err := checkSingleCAStore(&doc); err != nil {
			return nil, err
		}
		return &doc, nil
	}
	level := cfg.Level
	if level == "" {
		level = "strict"
	}
	if level == "audit" {
		// Defense in depth (config also rejects it): audit downgrades authenticity
		// to log-only, defeating the trust anchor.
		return nil, fmt.Errorf("verify level %q disables trust-anchor enforcement and is not allowed", level)
	}
	// "trust anything that chains to the configured CA(s), any identity."
	return &trustpolicy.Document{
		Version: "1.0",
		TrustPolicies: []trustpolicy.TrustPolicy{{
			Name:                  "gantry-default",
			RegistryScopes:        []string{"*"},
			SignatureVerification: trustpolicy.SignatureVerification{VerificationLevel: level},
			TrustStores:           []string{"ca:gantry"},
			TrustedIdentities:     []string{"*"},
		}},
	}, nil
}
