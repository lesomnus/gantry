package config

import "testing"

func TestRetentionMaxNValidation(t *testing.T) {
	cfg := func(keepN, maxN int) *Config {
		return &Config{Serve: ServeConfig{Retention: RetentionConfig{Path: "gc.db", KeepN: keepN, MaxN: maxN}}}
	}
	cases := []struct {
		name        string
		keepN, maxN int
		ok          bool
	}{
		{"max_n alone", 0, 5, true},
		{"max_n above keep_n", 3, 5, true},
		{"max_n equals keep_n", 5, 5, true},
		{"max_n below keep_n", 5, 3, false},
		{"negative max_n", 0, -1, false},
		{"both zero (disabled)", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := cfg(c.keepN, c.maxN).Evaluate()
			if c.ok && err != nil {
				t.Errorf("expected valid, got %v", err)
			}
			if !c.ok && err == nil {
				t.Error("expected a validation error")
			}
		})
	}
}
