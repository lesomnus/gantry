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

// source_wait is what collapses a burst of destinations onto one origin read, so
// it is on by default — but 0 is a meaningful value (waiting off), which is why
// the field is a pointer: unset and "explicitly zero" must not be the same thing.
func TestSourceWaitDefault(t *testing.T) {
	eval := func(t *testing.T, w WorkerConfig) time.Duration {
		t.Helper()
		c := Config{Worker: w}
		if err := c.Evaluate(); err != nil {
			t.Fatal(err)
		}
		return c.Worker.SourceWaitOr()
	}
	if got := eval(t, WorkerConfig{}); got != 30*time.Second {
		t.Errorf("unset = %s, want 30s", got)
	}
	zero := Duration(0)
	if got := eval(t, WorkerConfig{SourceWait: &zero}); got != 0 {
		t.Errorf("explicit 0 = %s, want waiting off", got)
	}
	five := Duration(5 * time.Second)
	if got := eval(t, WorkerConfig{SourceWait: &five}); got != 5*time.Second {
		t.Errorf("explicit 5s = %s", got)
	}
	// One worker is a serial pipeline: nothing can be filling anything while the
	// move that would wait for it is running, so the default must not park it.
	if got := eval(t, WorkerConfig{MaxConcurrentJobs: 1}); got != 0 {
		t.Errorf("single worker = %s, want waiting off by default", got)
	}
	if got := eval(t, WorkerConfig{MaxConcurrentJobs: 1, SourceWait: &five}); got != 5*time.Second {
		t.Errorf("single worker with an explicit value = %s, want it honored", got)
	}
}

// A scoped route is how a topology one-cache-per-source cannot express is written:
// several routes on the SAME origin, each naming the jobs it serves. First match
// wins, and a route nothing can reach is a config error rather than dead weight.
func TestCacheRouteSelection(t *testing.T) {
	load := func(t *testing.T, stores map[string]StoreConfig) (Config, error) {
		t.Helper()
		c := Config{Stores: stores}
		return c, c.Evaluate()
	}
	base := func() map[string]StoreConfig {
		return map[string]StoreConfig{
			"cloud": {Kind: "oci", Host: "cr.example.com", Caches: []CacheRoute{
				{Store: "rack1", ForTargets: []string{"node1"}},
				{Store: "team", ForRepos: []string{"team/**"}},
				{Store: "site"},
			}},
			"rack1": {Kind: "oci", Host: "cache.rack1"},
			"team":  {Kind: "oci", Host: "cache.team"},
			"site":  {Kind: "oci", Host: "registry.corp"},
			"node1": {Kind: "docker"},
			"node2": {Kind: "docker"},
		}
	}
	c, err := load(t, base())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tc := range []struct{ target, repo, want string }{
		{"node1", "other/app", "rack1"}, // target scope wins, and it is first
		{"node1", "team/app", "rack1"},  // …even when a later route also matches
		{"node2", "team/app", "team"},   // repo scope
		{"node2", "other/app", "site"},  // the unscoped default, last
	} {
		if got := c.Stores["cloud"].CacheFor(tc.target, tc.repo); got != tc.want {
			t.Errorf("CacheFor(%q, %q) = %q, want %q", tc.target, tc.repo, got, tc.want)
		}
	}
	// A store with no route at all routes nothing.
	if got := c.Stores["site"].CacheFor("node1", "team/app"); got != "" {
		t.Errorf("a store with no route returned %q", got)
	}

	// `cache: x` is exactly `caches: [{store: x}]`, normalized at load.
	c2, err := load(t, map[string]StoreConfig{
		"cloud": {Kind: "oci", Host: "cr.example.com", Cache: "site"},
		"site":  {Kind: "oci", Host: "registry.corp"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := c2.Stores["cloud"].CacheFor("anything", "any/repo"); got != "site" {
		t.Errorf("the shorthand resolved to %q, want site", got)
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string]StoreConfig)
		want   string
	}{
		{"both spellings", func(m map[string]StoreConfig) {
			s := m["cloud"]
			s.Cache = "site"
			m["cloud"] = s
		}, "not both"},
		{"unreachable route after an unscoped one", func(m map[string]StoreConfig) {
			s := m["cloud"]
			s.Caches = []CacheRoute{{Store: "site"}, {Store: "rack1", ForTargets: []string{"node1"}}}
			m["cloud"] = s
		}, "unreachable"},
		{"undeclared target scope", func(m map[string]StoreConfig) {
			s := m["cloud"]
			s.Caches = []CacheRoute{{Store: "site", ForTargets: []string{"nope"}}}
			m["cloud"] = s
		}, "not a declared store"},
		{"host-qualified repo pattern", func(m map[string]StoreConfig) {
			s := m["cloud"]
			s.Caches = []CacheRoute{{Store: "site", ForRepos: []string{"cr.example.com/team/**"}}}
			m["cloud"] = s
		}, "host-qualified"},
		{"route names no store", func(m map[string]StoreConfig) {
			s := m["cloud"]
			s.Caches = []CacheRoute{{ForRepos: []string{"team/**"}}}
			m["cloud"] = s
		}, "names no store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stores := base()
			tc.mutate(stores)
			_, err := load(t, stores)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// Retention rules are doublestar patterns over HOST-QUALIFIED repositories, and a
// routed job deliberately lands the image under the cache's host. Without the
// expansion a rule written for the origin matches nothing the node holds and the
// image sits unmanaged forever.
func TestRouteAliasesForRetentionRules(t *testing.T) {
	c := Config{Stores: map[string]StoreConfig{
		"cloud": {Kind: "oci", Host: "cr.example.com", Caches: []CacheRoute{
			{Store: "rack1", ForTargets: []string{"node1"}},
			{Store: "site"},
		}},
		"rack1": {Kind: "oci", Host: "cache.rack1"},
		"site":  {Kind: "oci", Host: "registry.corp"},
		"node1": {Kind: "docker"},
		"node2": {Kind: "docker"},
	}}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	got := c.RouteAliases("node1", "cr.example.com/team/**")
	want := []string{"cache.rack1/team/**", "registry.corp/team/**"}
	if len(got) != len(want) {
		t.Fatalf("aliases for node1 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("aliases for node1 = %v, want %v", got, want)
		}
	}
	// node2 is not in the rack-1 route's scope, so a rack-1 image can never reach
	// it and its rules must not claim one.
	if got := c.RouteAliases("node2", "cr.example.com/team/**"); len(got) != 1 || got[0] != "registry.corp/team/**" {
		t.Errorf("aliases for node2 = %v, want only the unscoped site route", got)
	}
	// A host-agnostic pattern already matches everywhere; a host that declares no
	// route has nothing to expand into.
	if got := c.RouteAliases("node1", "**"); got != nil {
		t.Errorf("host-agnostic pattern expanded to %v", got)
	}
	if got := c.RouteAliases("node1", "registry.corp/team/**"); got != nil {
		t.Errorf("a store with no route of its own expanded to %v", got)
	}
}
