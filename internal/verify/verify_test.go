package verify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
)

// writeTestCert drops a self-signed cert into dir under name and returns nothing.
func writeTestCert(t *testing.T, dir, name string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gantry-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, name), pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCertDir(t *testing.T) {
	dir := t.TempDir()
	writeTestCert(t, dir, "a.crt")
	writeTestCert(t, dir, "b.pem")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	certs, err := loadCertDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 2 {
		t.Errorf("loaded %d certs, want 2 (non-cert files ignored)", len(certs))
	}
}

func TestLoadTrustStoreErrors(t *testing.T) {
	t.Run("empty path", func(t *testing.T) {
		if _, err := loadTrustStore(config.VerifyConfig{}); err == nil {
			t.Error("empty trust_store should error")
		}
	})
	t.Run("system rejected", func(t *testing.T) {
		if _, err := loadTrustStore(config.VerifyConfig{TrustStore: "system"}); err == nil {
			t.Error(`"system" trust_store should be rejected in v1`)
		}
	})
	t.Run("no certs in dir", func(t *testing.T) {
		if _, err := loadTrustStore(config.VerifyConfig{TrustStore: t.TempDir()}); err == nil {
			t.Error("a dir with no CA certs should error")
		}
	})
	t.Run("ok", func(t *testing.T) {
		dir := t.TempDir()
		writeTestCert(t, dir, "ca.crt")
		if _, err := loadTrustStore(config.VerifyConfig{TrustStore: dir}); err != nil {
			t.Errorf("valid trust store errored: %v", err)
		}
	})
}

func TestSynthPolicyValidates(t *testing.T) {
	for _, level := range []string{"", "strict", "permissive"} {
		doc, err := loadOrSynthPolicy(config.VerifyConfig{Level: level})
		if err != nil {
			t.Fatalf("level %q: %v", level, err)
		}
		if err := doc.Validate(); err != nil {
			t.Errorf("synthesized policy (level %q) is invalid: %v", level, err)
		}
		if len(doc.TrustPolicies) != 1 || doc.TrustPolicies[0].TrustStores[0] != "ca:gantry" {
			t.Errorf("unexpected synth policy: %+v", doc.TrustPolicies)
		}
	}
}

// TestSynthRejectsAudit: audit disables the trust anchor, so the synthesized
// policy must refuse it (defense in depth beyond config validation).
func TestSynthRejectsAudit(t *testing.T) {
	if _, err := loadOrSynthPolicy(config.VerifyConfig{Level: "audit"}); err == nil {
		t.Error("audit level should be rejected for the synthesized policy")
	}
}

// TestCustomPolicyTrustStoreValidation: a custom trust_policy may reference only
// a single ca:<name> store (gantry's flat CA bundle cannot distinguish more).
func TestCustomPolicyTrustStoreValidation(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "tp.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const single = `{"version":"1.0","trustPolicies":[{"name":"a","registryScopes":["*"],"signatureVerification":{"level":"strict"},"trustStores":["ca:gantry"],"trustedIdentities":["*"]}]}`
	const multi = `{"version":"1.0","trustPolicies":[{"name":"a","registryScopes":["a/*"],"signatureVerification":{"level":"strict"},"trustStores":["ca:teamA"],"trustedIdentities":["*"]},{"name":"b","registryScopes":["b/*"],"signatureVerification":{"level":"strict"},"trustStores":["ca:teamB"],"trustedIdentities":["*"]}]}`
	const nonCA = `{"version":"1.0","trustPolicies":[{"name":"a","registryScopes":["*"],"signatureVerification":{"level":"strict"},"trustStores":["signingAuthority:x"],"trustedIdentities":["*"]}]}`

	if _, err := loadOrSynthPolicy(config.VerifyConfig{TrustPolicy: write(single)}); err != nil {
		t.Errorf("single ca store should be accepted: %v", err)
	}
	if _, err := loadOrSynthPolicy(config.VerifyConfig{TrustPolicy: write(multi)}); err == nil {
		t.Error("multiple distinct trust stores should be rejected")
	}
	if _, err := loadOrSynthPolicy(config.VerifyConfig{TrustPolicy: write(nonCA)}); err == nil {
		t.Error("non-ca trust store should be rejected")
	}
}

func TestNewFailFast(t *testing.T) {
	// Enabled but no trust material -> constructor must fail (so serve won't start).
	if _, err := New(config.VerifyConfig{Mode: config.VerifyRequire, Provider: "notation"}); err == nil {
		t.Error("New should fail when trust_store is missing")
	}
	// Unknown provider.
	dir := t.TempDir()
	writeTestCert(t, dir, "ca.crt")
	if _, err := New(config.VerifyConfig{Mode: config.VerifyRequire, Provider: "cosign", TrustStore: dir}); err == nil {
		t.Error("New should reject an unsupported provider")
	}
	// Valid.
	if _, err := New(config.VerifyConfig{Mode: config.VerifyRequire, Provider: "notation", TrustStore: dir, Level: "strict"}); err != nil {
		t.Errorf("New with valid config errored: %v", err)
	}
}

func TestVerifyEnabledStoreAware(t *testing.T) {
	// Global off, but a store overrides to require -> verification must be built.
	var c config.ServeConfig
	c.Verify.Mode = config.VerifyOff
	c.Stores = map[string]config.StoreConfig{
		"a": {Kind: "oci", Host: "a"},
		"b": {Kind: "oci", Host: "b", Verify: &config.StoreVerify{Mode: config.VerifyRequire}},
	}
	if !c.VerifyEnabled() {
		t.Error("a per-store require override should enable verification even when global mode is off")
	}
	// Nothing on anywhere -> disabled.
	c.Stores["b"] = config.StoreConfig{Kind: "oci", Host: "b"}
	if c.VerifyEnabled() {
		t.Error("no global mode and no store override should be disabled")
	}
}

func TestEffectiveMode(t *testing.T) {
	cfg := config.VerifyConfig{Mode: config.VerifyIfPresent}
	global := config.StoreConfig{Name: "a", Kind: "oci"}
	if got := cfg.EffectiveMode(global); got != config.VerifyIfPresent {
		t.Errorf("global default = %q, want verify-if-present", got)
	}
	override := config.StoreConfig{Name: "b", Kind: "oci", Verify: &config.StoreVerify{Mode: config.VerifyRequire}}
	if got := cfg.EffectiveMode(override); got != config.VerifyRequire {
		t.Errorf("per-store override = %q, want require", got)
	}
	off := config.StoreConfig{Name: "c", Kind: "oci", Verify: &config.StoreVerify{Mode: config.VerifyOff}}
	if got := cfg.EffectiveMode(off); got != config.VerifyOff {
		t.Errorf("per-store off = %q, want off", got)
	}
	// No global, no override -> off.
	if got := (config.VerifyConfig{}).EffectiveMode(global); got != config.VerifyOff {
		t.Errorf("unset global = %q, want off", got)
	}
}
