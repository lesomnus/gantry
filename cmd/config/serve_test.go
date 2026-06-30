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
	if c.Serve.Registry.Mode != "copy" {
		t.Errorf("mode = %q, want copy", c.Serve.Registry.Mode)
	}
	if c.Serve.Warm.MaxConcurrentJobs != 2 || c.Serve.Warm.MaxConcurrentLayers != 4 || c.Serve.Warm.QueueSize != 256 {
		t.Errorf("warm pool = %+v, want jobs=2 layers=4 queue=256", c.Serve.Warm)
	}
	if got := time.Duration(c.Serve.Warm.JobTTL); got != 30*time.Minute {
		t.Errorf("job_ttl = %v, want 30m", got)
	}
	if len(c.Serve.Registry.Rewrite) != 1 {
		t.Fatalf("default rewrite rules = %d, want 1", len(c.Serve.Registry.Rewrite))
	}
	r := c.Serve.Registry.Rewrite[0]
	if r.Pattern != "**" || r.Template != "{{.CacheHost}}/{{.Repo}}" {
		t.Errorf("default rule = %q -> %q", r.Pattern, r.Template)
	}
}

func TestServeConfigDecode(t *testing.T) {
	const src = `
serve:
  addr: ":9000"
  shutdown_grace: "5s"
  registry:
    mode: "proxy"
    host: "cache.example.com"
    insecure: true
    rewrite:
      - { "ghcr.io/**": "{{.CacheHost}}/{{.Repo}}" }
      - { "**": "{{.CacheHost}}/{{.Registry}}/{{.Repo}}" }
  warm:
    platforms: ["linux/arm64"]
    max_concurrent_jobs: 3
  targets:
    - { name: "d", kind: "docker", address: "/var/run/docker.sock" }
    - { name: "c", kind: "containerd", address: "/run/containerd/containerd.sock" }
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if c.Serve.Addr != ":9000" {
		t.Errorf("addr = %q", c.Serve.Addr)
	}
	if got := time.Duration(c.Serve.ShutdownGrace); got != 5*time.Second {
		t.Errorf("shutdown_grace = %v", got)
	}
	if c.Serve.Registry.Mode != "proxy" || c.Serve.Registry.Host != "cache.example.com" {
		t.Errorf("registry = %+v", c.Serve.Registry)
	}
	if len(c.Serve.Registry.Rewrite) != 2 {
		t.Fatalf("rewrite rules = %d, want 2", len(c.Serve.Registry.Rewrite))
	}
	if r := c.Serve.Registry.Rewrite[0]; r.Pattern != "ghcr.io/**" {
		t.Errorf("rule[0] pattern = %q", r.Pattern)
	}
	if c.Serve.Warm.MaxConcurrentJobs != 3 {
		t.Errorf("max_concurrent_jobs = %d, want 3 (explicit, not defaulted)", c.Serve.Warm.MaxConcurrentJobs)
	}
	if len(c.Serve.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(c.Serve.Targets))
	}
	if ns := c.Serve.Targets[1].Namespace; ns != "k8s.io" {
		t.Errorf("containerd namespace = %q, want k8s.io default", ns)
	}
}

func TestRewriteRuleRender(t *testing.T) {
	data := struct {
		CacheHost string
		Repo      string
	}{CacheHost: "cache.local", Repo: "library/redis"}
	t.Run("identity template renders whole ref", func(t *testing.T) {
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
serve:
  registry:
    rewrite:
      - { "a/**": "x", "b/**": "y" }
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err == nil {
		t.Error("expected error decoding a multi-key rewrite rule")
	}
}
