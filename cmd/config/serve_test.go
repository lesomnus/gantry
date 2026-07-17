package config

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

func TestEvaluateDefaults(t *testing.T) {
	var c Config
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if c.Serve.Addr != ":8080" {
		t.Errorf("addr = %q, want :8080", c.Serve.Addr)
	}
	if got := time.Duration(c.Serve.ShutdownGrace); got != 15*time.Second {
		t.Errorf("shutdown_grace = %v, want 15s", got)
	}
	if c.Worker.MaxConcurrentJobs != 2 || c.Worker.MaxConcurrentLayers != 4 || c.Worker.QueueSize != 256 {
		t.Errorf("worker pool = %+v, want jobs=2 layers=4 queue=256", c.Worker)
	}
	if got := time.Duration(c.Worker.JobTTL); got != 30*time.Minute {
		t.Errorf("job_ttl = %v, want 30m", got)
	}
	if got := time.Duration(c.Serve.Health.CacheTTL); got != 5*time.Second {
		t.Errorf("health.cache_ttl = %v, want 5s", got)
	}
	if got := time.Duration(c.Serve.Health.ProbeTimeout); got != 3*time.Second {
		t.Errorf("health.probe_timeout = %v, want 3s", got)
	}
	if c.Serve.AllowUnknownStores {
		t.Error("allow_unknown_stores should default to false")
	}
}

func TestStoresDecode(t *testing.T) {
	const src = `
serve:
  allow_unknown_stores: true
stores:
  dockerhub: { kind: "oci", host: "docker.io" }
  "ghcr.io": { kind: "oci" }
  cache:
    kind: "oci"
    host: "cache.local:5000"
    insecure: true
    downstream_host: "cache.cr.com"
    rewrite:
      - { "ghcr.io/**": "{{.CacheHost}}/{{.Repo}}" }
  k3s: { kind: "containerd", address: "/run/containerd.sock", namespace: "k8s.io" }
  nomad: { kind: "docker", address: "/var/run/docker.sock" }
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !c.Serve.AllowUnknownStores {
		t.Error("allow_unknown_stores = false, want true")
	}
	if len(c.Stores) != 5 {
		t.Fatalf("stores = %d, want 5", len(c.Stores))
	}
	ss := c.Stores
	if dh := ss["dockerhub"]; dh.Name != "dockerhub" || dh.Mode != "copy" || len(dh.Rewrite) != 1 {
		t.Errorf("dockerhub registry defaults = %+v", dh)
	}
	if g := ss["ghcr.io"]; g.Host != "ghcr.io" {
		t.Errorf("host should default to store name, got %q", g.Host)
	}
	if cache := ss["cache"]; cache.DownstreamHost != "cache.cr.com" || cache.Rewrite[0].Pattern != "ghcr.io/**" {
		t.Errorf("cache = %+v", cache)
	}
	if k := ss["k3s"]; !k.IsEngine() || k.Namespace != "k8s.io" {
		t.Errorf("k3s = %+v", k)
	}
	if !ss["nomad"].IsEngine() || !ss["cache"].IsRegistry() {
		t.Error("kind predicates wrong")
	}
}

func TestStoreValidation(t *testing.T) {
	t.Run("unknown kind", func(t *testing.T) {
		var c Config
		c.Stores = map[string]StoreConfig{"x": {Kind: "bogus"}}
		if err := c.Evaluate(); err == nil {
			t.Error("expected unknown-kind error")
		}
	})
	t.Run("empty name key", func(t *testing.T) {
		var c Config
		c.Stores = map[string]StoreConfig{"": {Kind: "oci"}}
		if err := c.Evaluate(); err == nil {
			t.Error("expected empty-name error")
		}
	})
}

func TestRewriteRuleRender(t *testing.T) {
	data := struct {
		CacheHost string
		Repo      string
	}{CacheHost: "cache.local", Repo: "library/redis"}
	t.Run("relocate by repo", func(t *testing.T) {
		r := RewriteRule{Pattern: "**", Template: "{{.CacheHost}}/{{.Repo}}"}
		if err := r.compile(); err != nil {
			t.Fatalf("compile: %v", err)
		}
		out, err := r.Render(data)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if out != "cache.local/library/redis" {
			t.Errorf("render = %q", out)
		}
	})
	t.Run("uncompiled rule errors", func(t *testing.T) {
		r := RewriteRule{Pattern: "**", Template: "{{.Repo}}"}
		if _, err := r.Render(data); err == nil {
			t.Error("expected error rendering an uncompiled rule")
		}
	})
	t.Run("bad template fails to compile", func(t *testing.T) {
		r := RewriteRule{Pattern: "**", Template: "{{.Repo"}
		if err := r.compile(); err == nil {
			t.Error("expected compile error for malformed template")
		}
	})
}

func TestRewriteRuleDecodeRejectsMultiKey(t *testing.T) {
	const src = `
stores:
  cache:
    kind: "oci"
    host: "cache.local"
    rewrite:
      - { "a/**": "x", "b/**": "y" }
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err == nil {
		t.Error("expected error decoding a multi-key rewrite rule")
	}
}
