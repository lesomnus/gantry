package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/gantry/cmd/config"
)

func TestSwappableDescribeAndReload(t *testing.T) {
	dir := t.TempDir()
	writeTestCert(t, dir, "ca1.pem")
	cfg := config.VerifyConfig{Mode: config.VerifyRequire, TrustStore: dir, Level: "strict"}

	s, err := NewSwappable(cfg)
	if err != nil {
		t.Fatal(err)
	}
	desc := s.Describe()
	if !desc.Enabled || desc.Provider != "notation" || desc.Mode != "require" {
		t.Fatalf("describe = %+v", desc)
	}
	if len(desc.Anchors) != 1 || desc.Anchors[0].Subject == "" ||
		len(desc.Anchors[0].Fingerprint) != 64 || desc.Anchors[0].NotAfter.IsZero() {
		t.Fatalf("anchors = %+v, want one fingerprinted anchor with expiry", desc.Anchors)
	}
	if len(desc.Policies) != 1 || desc.Policies[0].Level != "strict" {
		t.Fatalf("policies = %+v, want the synthesized strict policy", desc.Policies)
	}

	// Rotation: a second CA appears on disk, then Reload picks it up.
	writeTestCert(t, dir, "ca2.pem")
	desc, err = s.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Anchors) != 2 {
		t.Fatalf("anchors after reload = %d, want 2", len(desc.Anchors))
	}

	// A broken reload must keep the previous verifier active.
	for _, n := range []string{"ca1.pem", "ca2.pem"} {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Reload(); err == nil {
		t.Fatal("reload with an empty trust store must fail")
	}
	if got := s.Describe(); len(got.Anchors) != 2 {
		t.Errorf("failed reload must retain the previous verifier, got %d anchors", len(got.Anchors))
	}
}
