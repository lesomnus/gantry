package enforce

import (
	"strings"
	"testing"
)

func TestResolveSelfIDPriority(t *testing.T) {
	// explicit wins over everything
	if got := resolveSelfID("explicit-id"); got != "explicit-id" {
		t.Errorf("explicit self_container = %q, want explicit-id", got)
	}
	// GANTRY_SELF_ID env is used when no explicit value
	t.Setenv("GANTRY_SELF_ID", "env-id")
	if got := resolveSelfID(""); got != "env-id" {
		t.Errorf("env fallback = %q, want env-id", got)
	}
	// explicit still wins over the env
	if got := resolveSelfID("explicit-id"); got != "explicit-id" {
		t.Errorf("explicit should win over env, got %q", got)
	}
}

func TestLooksLikeContainerID(t *testing.T) {
	yes := []string{"0123456789ab", strings.Repeat("a", 64), "abcdef012345"}
	for _, s := range yes {
		if !looksLikeContainerID(s) {
			t.Errorf("looksLikeContainerID(%q) = false, want true", s)
		}
	}
	no := []string{"", "short", "0123456789", "gantry-prod-01", "ABCDEF012345", "0123456789ag"}
	for _, s := range no {
		if looksLikeContainerID(s) {
			t.Errorf("looksLikeContainerID(%q) = true, want false", s)
		}
	}
}

func TestIsSelf(t *testing.T) {
	full := strings.Repeat("a", 64)
	short := full[:12]

	// self resolved to the short id (hostname default); the event carries the full id
	if !(selfGuard{id: short}).isSelf(full) {
		t.Error("short self id should prefix-match the full event id")
	}
	// self resolved to the full id; a (rare) short event id still matches
	if !(selfGuard{id: full}).isSelf(short) {
		t.Error("full self id should reverse-prefix-match a short event id")
	}
	// a different container is not self
	if (selfGuard{id: short}).isSelf(strings.Repeat("b", 64)) {
		t.Error("a different container must not match")
	}
	// empty guards: unknown self id, or empty event id, never match
	if (selfGuard{}).isSelf(full) {
		t.Error("an unresolved self id must never match")
	}
	if (selfGuard{id: short}).isSelf("") {
		t.Error("an empty container id must never match")
	}
}
