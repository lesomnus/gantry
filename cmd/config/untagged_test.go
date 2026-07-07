package config

import (
	"testing"
	"time"
)

func TestUntaggedAfterDefaultsAndValidation(t *testing.T) {
	d := func(v time.Duration) *Duration { x := Duration(v); return &x }
	base := func(kind string, r *StoreRetention) *Config {
		return &Config{Stores: map[string]StoreConfig{"s": {Kind: kind, Retention: r}}}
	}

	t.Run("docker defaults to 1h", func(t *testing.T) {
		c := base("docker", &StoreRetention{Path: "gc.db"})
		if err := c.Evaluate(); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got := c.Stores["s"].Retention.UntaggedReapAfter(); got != time.Hour {
			t.Errorf("untagged_after default = %v; want 1h", got)
		}
	})
	t.Run("explicit 0s disables", func(t *testing.T) {
		c := base("docker", &StoreRetention{Path: "gc.db", UntaggedAfter: d(0)})
		if err := c.Evaluate(); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got := c.Stores["s"].Retention.UntaggedReapAfter(); got != 0 {
			t.Errorf("untagged_after = %v; want 0 (disabled)", got)
		}
	})
	t.Run("negative rejected", func(t *testing.T) {
		c := base("docker", &StoreRetention{Path: "gc.db", UntaggedAfter: d(-time.Minute)})
		if err := c.Evaluate(); err == nil {
			t.Error("expected a negative untagged_after to be rejected")
		}
	})
	t.Run("containerd with the knob rejected", func(t *testing.T) {
		c := base("containerd", &StoreRetention{Path: "gc.db", UntaggedAfter: d(time.Hour)})
		if err := c.Evaluate(); err == nil {
			t.Error("expected untagged_after on a containerd store to be rejected")
		}
	})
	t.Run("containerd without the knob stays off", func(t *testing.T) {
		c := base("containerd", &StoreRetention{Path: "gc.db"})
		if err := c.Evaluate(); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got := c.Stores["s"].Retention.UntaggedReapAfter(); got != 0 {
			t.Errorf("untagged_after = %v; want 0 on containerd", got)
		}
	})
}

func TestUntaggedReapSharedDaemonRejected(t *testing.T) {
	d := func(v time.Duration) *Duration { x := Duration(v); return &x }
	rt := func(path string, after *Duration) *StoreRetention {
		return &StoreRetention{Path: path, UntaggedAfter: after}
	}

	// Two docker stores on the same daemon (same address; "" = default socket),
	// both reaping (default-on): rejected — independent reap clocks and pins
	// over one image store.
	c := &Config{Stores: map[string]StoreConfig{
		"a": {Kind: "docker", Retention: rt("/a.db", nil)},
		"b": {Kind: "docker", Retention: rt("/b.db", nil)},
	}}
	if err := c.Evaluate(); err == nil {
		t.Error("expected two reapers on the same daemon to be rejected")
	}

	// Equivalent spellings of one daemon are still the same daemon.
	for _, pair := range [][2]string{
		{"", "/var/run/docker.sock"},
		{"", "unix:///var/run/docker.sock"},
		{"/run/x.sock", "unix:///run/x.sock"},
	} {
		c := &Config{Stores: map[string]StoreConfig{
			"a": {Kind: "docker", Address: pair[0], Retention: rt("/a.db", nil)},
			"b": {Kind: "docker", Address: pair[1], Retention: rt("/b.db", nil)},
		}}
		if err := c.Evaluate(); err == nil {
			t.Errorf("addresses %q and %q are the same daemon; expected rejection", pair[0], pair[1])
		}
	}

	// Distinct daemons are fine.
	ok := &Config{Stores: map[string]StoreConfig{
		"a": {Kind: "docker", Address: "/run/a.sock", Retention: rt("/a.db", nil)},
		"b": {Kind: "docker", Address: "/run/b.sock", Retention: rt("/b.db", nil)},
	}}
	if err := ok.Evaluate(); err != nil {
		t.Errorf("distinct daemons should validate: %v", err)
	}

	// Same daemon with one reaper turned off is fine.
	ok = &Config{Stores: map[string]StoreConfig{
		"a": {Kind: "docker", Retention: rt("/a.db", nil)},
		"b": {Kind: "docker", Retention: rt("/b.db", d(0))},
	}}
	if err := ok.Evaluate(); err != nil {
		t.Errorf("a single reaper per daemon should validate: %v", err)
	}
}
