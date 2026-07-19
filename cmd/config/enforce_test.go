package config

import (
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

// evalYAML decodes (strict) and evaluates a config from YAML, as startup does.
func evalYAML(t *testing.T, src string) (*Config, error) {
	t.Helper()
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(src), yaml.DisallowUnknownField())
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	return &c, c.Evaluate()
}

const baseStores = `
stores:
  local:
    kind: oci
    host: "127.0.0.1:5000"
    insecure: true
  dockerd:
    kind: docker
    address: "unix:///var/run/docker.sock"
`

func TestEnforceConfigValidAndDefaults(t *testing.T) {
	src := baseStores + `
serve:
  verify:
    trust_store: /etc/gantry/ca
    cache:
      path: /var/lib/gantry/verify.db
  enforce:
    mode: quarantine
    stores: [dockerd]
`
	c, err := evalYAML(t, src)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !c.Serve.Enforce.Enabled() {
		t.Fatal("enforce should be enabled")
	}
	if c.Serve.Enforce.OnUnavailable != "grace" {
		t.Errorf("on_unavailable default = %q, want grace", c.Serve.Enforce.OnUnavailable)
	}
	if !c.NeedVerifier() {
		t.Error("NeedVerifier should be true when enforce is on even with verify.mode off")
	}
	if c.VerifyEnabled() {
		t.Error("VerifyEnabled should be false (admission mode off)")
	}
	// verifier defaults applied via NeedVerifier gate
	if c.Serve.Verify.Provider != "notation" || c.Serve.Verify.Level != "strict" {
		t.Errorf("verifier defaults not applied: provider=%q level=%q", c.Serve.Verify.Provider, c.Serve.Verify.Level)
	}
	// cache defaults 4w / 2w
	if time.Duration(c.Serve.Verify.Cache.TTL) != 28*24*time.Hour {
		t.Errorf("cache ttl default = %v, want 4w", time.Duration(c.Serve.Verify.Cache.TTL))
	}
	if time.Duration(c.Serve.Verify.Cache.Refresh) != 14*24*time.Hour {
		t.Errorf("cache refresh default = %v, want 2w", time.Duration(c.Serve.Verify.Cache.Refresh))
	}
}

func TestEnforceConfigRejections(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown mode",
			yaml: baseStores + "\nserve:\n  enforce:\n    mode: quarintine\n",
			want: "enforce.mode",
		},
		{
			name: "unknown on_unavailable",
			yaml: baseStores + "\nserve:\n  verify:\n    trust_store: /ca\n    cache: {path: /v.db}\n  enforce:\n    mode: quarantine\n    stores: [dockerd]\n    on_unavailable: maybe\n",
			want: "on_unavailable",
		},
		{
			name: "empty stores",
			yaml: baseStores + "\nserve:\n  verify:\n    trust_store: /ca\n    cache: {path: /v.db}\n  enforce:\n    mode: quarantine\n    stores: []\n",
			want: "must name at least one engine store",
		},
		{
			name: "unknown store",
			yaml: baseStores + "\nserve:\n  verify:\n    trust_store: /ca\n    cache: {path: /v.db}\n  enforce:\n    mode: quarantine\n    stores: [nope]\n",
			want: "unknown store",
		},
		{
			name: "non-engine store",
			yaml: baseStores + "\nserve:\n  verify:\n    trust_store: /ca\n    cache: {path: /v.db}\n  enforce:\n    mode: quarantine\n    stores: [local]\n",
			want: "supports only docker",
		},
		{
			name: "containerd store rejected",
			yaml: "stores:\n  ctr:\n    kind: containerd\n    address: \"/run/containerd/containerd.sock\"\nserve:\n  verify:\n    trust_store: /ca\n    cache: {path: /v.db}\n  enforce:\n    mode: quarantine\n    stores: [ctr]\n",
			want: "supports only docker",
		},
		{
			name: "enforce without cache",
			yaml: baseStores + "\nserve:\n  verify:\n    trust_store: /ca\n  enforce:\n    mode: quarantine\n    stores: [dockerd]\n",
			want: "requires serve.verify.cache.path",
		},
		{
			name: "enforce without trust_store",
			yaml: baseStores + "\nserve:\n  verify:\n    cache: {path: /v.db}\n  enforce:\n    mode: quarantine\n    stores: [dockerd]\n",
			want: "requires serve.verify.trust_store",
		},
		{
			name: "refresh greater than ttl",
			yaml: baseStores + "\nserve:\n  verify:\n    cache:\n      path: /v.db\n      ttl: 1w\n      refresh: 2w\n",
			want: "refresh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := evalYAML(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestBboltPathCollision(t *testing.T) {
	// cache.path colliding with a retention.path must be rejected.
	src := `
stores:
  dockerd:
    kind: docker
    address: "unix:///var/run/docker.sock"
    retention:
      path: /var/lib/gantry/shared.db
serve:
  verify:
    trust_store: /ca
    cache:
      path: /var/lib/gantry/shared.db
  enforce:
    mode: quarantine
    stores: [dockerd]
`
	_, err := evalYAML(t, src)
	if err == nil || !strings.Contains(err.Error(), "share bbolt path") {
		t.Fatalf("expected bbolt path collision error, got %v", err)
	}
}

func TestEnforceOffIsNoop(t *testing.T) {
	// mode off (or unset) needs none of the enforce prerequisites.
	src := baseStores + "\nserve:\n  enforce:\n    mode: off\n"
	c, err := evalYAML(t, src)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if c.Serve.Enforce.Enabled() {
		t.Error("enforce mode off should not be enabled")
	}
	if c.NeedVerifier() {
		t.Error("NeedVerifier should be false when nothing needs a verifier")
	}
}

func TestVerifyCacheShortTTLDefaultsRefreshBelowTTL(t *testing.T) {
	// ttl shorter than the 2w refresh default, refresh unset: must NOT self-reject.
	src := baseStores + "\nserve:\n  verify:\n    trust_store: /ca\n    mode: require\n    cache:\n      path: /v.db\n      ttl: 1w\n"
	c, err := evalYAML(t, src)
	if err != nil {
		t.Fatalf("a short ttl with unset refresh should be valid, got: %v", err)
	}
	if got := time.Duration(c.Serve.Verify.Cache.Refresh); got != 7*24*time.Hour {
		t.Errorf("refresh should default down to ttl (1w), got %v", got)
	}
}

func TestLocalLayoutEnabled(t *testing.T) {
	src := baseStores + "\nserve:\n  verify:\n    trust_store: /ca\n    local_layout: /etc/gantry/sig\n    mode: require\n"
	c, err := evalYAML(t, src)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !c.Serve.Verify.LocalLayoutEnabled() {
		t.Error("LocalLayoutEnabled should be true")
	}
}
