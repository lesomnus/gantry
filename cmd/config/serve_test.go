package config

import (
	"strings"
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
	if dh := ss["dockerhub"]; dh.Name != "dockerhub" || dh.Mode != "copy" {
		t.Errorf("dockerhub registry defaults = %+v", dh)
	}
	if g := ss["ghcr.io"]; g.Host != "ghcr.io" {
		t.Errorf("host should default to store name, got %q", g.Host)
	}
	if cache := ss["cache"]; cache.DownstreamHost != "cache.cr.com" || !cache.Insecure {
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

// A store's `cache` is a route to another registry that holds its content. It is
// resolved across stores, so it is validated after every store is known.
func TestStoreCacheValidation(t *testing.T) {
	load := func(t *testing.T, stores map[string]StoreConfig) error {
		t.Helper()
		c := Config{Stores: stores}
		return c.Evaluate()
	}
	reg := func(host string) StoreConfig { return StoreConfig{Kind: "oci", Host: host} }

	t.Run("a registry cached by another registry", func(t *testing.T) {
		if err := load(t, map[string]StoreConfig{
			"cloud": {Kind: "oci", Host: "cr.example.com", Cache: "site"},
			"site":  reg("registry.corp"),
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("a store that is a cache may declare its own", func(t *testing.T) {
		// Routing is one level deep, so this is two independent routes rather
		// than a chain — and never a cycle.
		if err := load(t, map[string]StoreConfig{
			"cloud": {Kind: "oci", Host: "cr.example.com", Cache: "site"},
			"site":  {Kind: "oci", Host: "registry.corp", Cache: "rack"},
			"rack":  reg("cache.rack1"),
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	for _, tc := range []struct {
		name   string
		stores map[string]StoreConfig
		want   string
	}{
		{
			"undeclared cache",
			map[string]StoreConfig{"cloud": {Kind: "oci", Host: "cr.example.com", Cache: "nope"}},
			"not a declared store",
		},
		{
			"cache names itself",
			map[string]StoreConfig{"cloud": {Kind: "oci", Host: "cr.example.com", Cache: "cloud"}},
			"names the store itself",
		},
		{
			"cache is an engine",
			map[string]StoreConfig{
				"cloud": {Kind: "oci", Host: "cr.example.com", Cache: "edge"},
				"edge":  {Kind: "docker"},
			},
			"only a registry can hold copies",
		},
		{
			"declared on an engine",
			map[string]StoreConfig{
				"edge": {Kind: "docker", Cache: "site"},
				"site": reg("registry.corp"),
			},
			"never a job's source",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := load(t, tc.stores)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}
