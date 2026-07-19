package enforce

import (
	"os"
	"regexp"
	"strings"
)

// selfGuard identifies gantry's own container so runtime enforcement never
// removes the container gantry runs in. It is a safety interlock, NOT a security
// boundary — an attacker cannot make their container be gantry's — and it is
// matched by container identity, never by image name.
type selfGuard struct {
	id string // gantry's own container id (full or short); "" when unknown
}

// hexID matches a docker/containerd 64-hex container id anywhere in a string
// (e.g. a cgroup path "/docker/<id>" or "/system.slice/docker-<id>.scope").
var hexID = regexp.MustCompile(`[0-9a-f]{64}`)

// resolveSelfID determines gantry's own container id, in priority order:
//
//  1. an explicit id/name (serve.enforce.self_container or GANTRY_SELF_ID);
//  2. the hostname — docker's default is the 12-char short id — but only when it
//     looks like a container id (a custom --hostname is ignored);
//  3. /proc/self/cgroup — works on cgroup v1 / cgroupns=host, but yields nothing
//     under cgroup v2 + cgroupns=private (the modern default), where it is "0::/".
//
// Returns "" when it cannot be determined; the caller logs and disables the
// interlock (gantry's own image should be signed and trusted anyway).
func resolveSelfID(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("GANTRY_SELF_ID"); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && looksLikeContainerID(h) {
		return h
	}
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		if id := hexID.FindString(string(b)); id != "" {
			return id
		}
	}
	return ""
}

// looksLikeContainerID reports whether s is a hex id of at least the docker
// short-id length (12), so a human-set --hostname is not mistaken for one.
func looksLikeContainerID(s string) bool {
	if len(s) < 12 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// isSelf reports whether containerID is gantry's own container. The event carries
// the full 64-hex id; the resolved self id may be the short (12-hex) form, so a
// prefix match in either direction covers both.
func (g selfGuard) isSelf(containerID string) bool {
	if g.id == "" || containerID == "" {
		return false
	}
	return strings.HasPrefix(containerID, g.id) || strings.HasPrefix(g.id, containerID)
}
