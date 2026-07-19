//go:build unix

// Package dockertest holds helpers shared by the live docker/containerd
// integration tests across packages.
package dockertest

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// procMu serializes lock acquisition within one test process; the file lock
// serializes across processes (parallel package test binaries under `go test
// ./...`).
var procMu sync.Mutex

// Lock serializes live docker/containerd integration tests that share the single
// daemon on the machine. `go test ./...` runs package test binaries in parallel,
// so the live tests in internal/down, internal/enforce, and internal/retention
// would otherwise mutate the same images (alpine, busybox), containers, and the
// shared containerd content store concurrently — racing on pulls, removals, and
// "image in use" / "removal already in progress". An exclusive advisory file
// lock, released when the test ends, makes each such test hold the daemon
// exclusively, across packages/processes and for a local `go test ./...` too.
//
// Call it ONCE per test, right after the daemon-availability skip: the underlying
// mutex is not reentrant, and skipping before the lock keeps a daemon-less machine
// from blocking. The `e2e`-tagged L2/L3 suites are not covered — they run in their
// own isolated single-package CI jobs and never share the daemon with these.
func Lock(t *testing.T) {
	t.Helper()
	procMu.Lock()
	path := filepath.Join(os.TempDir(), "gantry-docker-it.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		procMu.Unlock()
		t.Fatalf("dockertest: open lock %s: %v", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		procMu.Unlock()
		t.Fatalf("dockertest: flock %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		procMu.Unlock()
	})
}
