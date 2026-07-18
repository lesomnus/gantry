//go:build e2e_infra

// The L3-infra tier (docs/e2e-plan.md): run the shipped binary against the
// Ansible-provisioned registry matrix (ansible/), exercising the transport paths
// the hermetic tiers cannot — a real TLS registry (ca_cert) and the pull-through
// proxy. Self-skips unless GANTRY_E2E_CONFIG points at the discovery file the
// ansible `discovery` role emits.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/lesomnus/gantry/pb"
)

type discovery struct {
	GantryConfig string            `json:"gantry_config"`
	TrustStore   string            `json:"trust_store"`
	SignedRef    string            `json:"signed_ref"`
	Stores       map[string]string `json:"stores"`
}

func loadDiscovery(t *testing.T) discovery {
	t.Helper()
	p := os.Getenv("GANTRY_E2E_CONFIG")
	if p == "" {
		t.Skip("GANTRY_E2E_CONFIG unset; run `make e2e-infra` to provision the ansible/ env")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read discovery %s: %v", p, err)
	}
	var d discovery
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse discovery: %v", err)
	}
	return d
}

// TestL3Infra drives the shipped binary against the provisioned matrix: a plain
// copy and a copy into the TLS cache (ca_cert), proving the non-hermetic
// transport paths end to end.
func TestL3Infra(t *testing.T) {
	d := loadDiscovery(t)
	bin := buildGantry(t)
	const addr = "127.0.0.1:18080" // matches the rendered gantry.yaml
	_, client, stop := runGantry(t, bin, d.GantryConfig, addr)
	defer stop()

	// remote → cache over plain HTTP.
	j, err := client.Job().Add(context.Background(), copyReq("remote", "cache"))
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	waitTerminal(t, client, j.GetId())

	// remote → cache-tls over HTTPS, trusting the private CA via ca_cert.
	j, err = client.Job().Add(context.Background(), pb.JobAddRequest_builder{
		Ref: d.SignedRef, Source: pb.StoreByName("remote"), Target: pb.StoreByName("cache-tls"),
	}.Build())
	if err != nil {
		t.Fatalf("tls copy: %v", err)
	}
	waitTerminal(t, client, j.GetId())
}
