package config

import (
	"testing"
	"time"
)

func TestStoreRetentionValidation(t *testing.T) {
	i := func(v int) *int { return &v }
	base := func(kind string, r *StoreRetention) *Config {
		return &Config{Stores: map[string]StoreConfig{"s": {Kind: kind, Retention: r}}}
	}
	cases := []struct {
		name string
		cfg  *Config
		ok   bool
	}{
		{"retention on oci rejected", base("oci", &StoreRetention{Path: "gc.db"}), false},
		{"path required", base("docker", &StoreRetention{}), false},
		{"invalid repo pattern", base("docker", &StoreRetention{Path: "gc.db", Rules: []RetentionRule{{Repo: "[bad"}}}), false},
		{"invalid pin pattern", base("docker", &StoreRetention{Path: "gc.db", Rules: []RetentionRule{{Repo: "**", Pins: []string{"[bad"}}}}), false},
		{"empty repo", base("docker", &StoreRetention{Path: "gc.db", Rules: []RetentionRule{{Repo: ""}}}), false},
		{"max_n below keep_n", base("docker", &StoreRetention{Path: "gc.db", Rules: []RetentionRule{{Repo: "**", KeepN: i(5), MaxN: i(3)}}}), false},
		{"negative max_n", base("docker", &StoreRetention{Path: "gc.db", Rules: []RetentionRule{{Repo: "**", MaxN: i(-1)}}}), false},
		{"valid", base("containerd", &StoreRetention{Path: "gc.db", Rules: []RetentionRule{{Repo: "**", KeepN: i(2), MaxN: i(10)}}}), true},
		{"no retention (nil) ok", base("docker", nil), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Evaluate()
			if c.ok && err != nil {
				t.Errorf("expected valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}

func TestStoreRetentionDefaults(t *testing.T) {
	c := &Config{Stores: map[string]StoreConfig{
		"k3s": {Kind: "containerd", Retention: &StoreRetention{
			Path:  "/var/lib/gantry/k3s.db",
			Rules: []RetentionRule{{Repo: "**"}},
		}},
	}}
	if err := c.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	r := c.Stores["k3s"].Retention
	if time.Duration(r.Interval) != time.Hour {
		t.Errorf("interval default = %v; want 1h", time.Duration(r.Interval))
	}
	if time.Duration(r.MinInterval) != time.Minute {
		t.Errorf("min_interval default = %v; want 1m", time.Duration(r.MinInterval))
	}
	if time.Duration(r.Grace) != time.Hour {
		t.Errorf("grace default = %v; want 1h", time.Duration(r.Grace))
	}
	if !r.Enabled() {
		t.Error("Enabled() should be true")
	}
}

func TestStoreRetentionRejectsSharedPath(t *testing.T) {
	rt := func() *StoreRetention {
		return &StoreRetention{Path: "/var/lib/gantry/shared.db", Rules: []RetentionRule{{Repo: "**"}}}
	}
	c := &Config{Stores: map[string]StoreConfig{
		"a": {Kind: "docker", Retention: rt()},
		"b": {Kind: "containerd", Retention: rt()},
	}}
	if err := c.Evaluate(); err == nil {
		t.Error("expected two stores sharing a retention.path to be rejected")
	}

	// Distinct paths are fine.
	ok := &Config{Stores: map[string]StoreConfig{
		"a": {Kind: "docker", Retention: &StoreRetention{Path: "/a.db", Rules: []RetentionRule{{Repo: "**"}}}},
		"b": {Kind: "containerd", Retention: &StoreRetention{Path: "/b.db", Rules: []RetentionRule{{Repo: "**"}}}},
	}}
	if err := ok.Evaluate(); err != nil {
		t.Errorf("distinct retention paths should validate: %v", err)
	}
}
