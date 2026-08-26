package config

import "testing"

// evaluate runs a config with one store through Evaluate, which is where the
// store's own fields are checked.
func evaluate(t *testing.T, s StoreConfig) error {
	t.Helper()

	c := &Config{Stores: map[string]StoreConfig{"reg": s}}

	return c.Evaluate()
}

// token_file and username/password are two answers to one question, and a
// config that sets both leaves "which one authenticated" to be discovered from
// the registry's logs.
func TestTokenFileExcludesBasic(t *testing.T) {
	for _, tc := range []struct {
		what string
		s    StoreConfig
		ok   bool
	}{
		{"token_file alone", StoreConfig{Kind: "oci", TokenFile: "/run/token"}, true},
		{"username/password alone", StoreConfig{Kind: "oci", Username: "u", Password: "p"}, true},
		{"neither", StoreConfig{Kind: "oci"}, true},
		{"token_file and username", StoreConfig{Kind: "oci", TokenFile: "/run/token", Username: "u"}, false},
		{"token_file and password", StoreConfig{Kind: "oci", TokenFile: "/run/token", Password: "p"}, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			err := evaluate(t, tc.s)
			if tc.ok && err != nil {
				t.Errorf("should validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// An engine is *told* to pull; it authenticates to the registry with its own
// configuration, not with gantry's. A token_file on one is a credential that
// would never be sent, which is worth an error rather than silence.
func TestTokenFileIsNotForEngines(t *testing.T) {
	for _, kind := range []string{"docker", "containerd"} {
		t.Run(kind, func(t *testing.T) {
			if err := evaluate(t, StoreConfig{Kind: kind, TokenFile: "/run/token"}); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// The path is not a secret and is worth seeing in `gantry config`; what it
// points at never enters the configuration at all, which is the point of it.
func TestTheTokenPathIsNotRedacted(t *testing.T) {
	c := &Config{Stores: map[string]StoreConfig{
		"reg": {Kind: "oci", TokenFile: "/run/hday/token", Password: "hunter2"},
	}}

	got := c.Redacted().Stores["reg"]
	if got.TokenFile != "/run/hday/token" {
		t.Errorf("TokenFile = %q, want the path", got.TokenFile)
	}
	if got.Password == "hunter2" {
		t.Error("the password should still be masked")
	}
}
