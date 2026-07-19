//go:build !unix

package dockertest

import (
	"sync"
	"testing"
)

var procMu sync.Mutex

// Lock falls back to process-level serialization on platforms without flock.
// The live docker/containerd tests only run on unix (they need a daemon socket),
// so cross-process locking is never exercised here.
func Lock(t *testing.T) {
	t.Helper()
	procMu.Lock()
	t.Cleanup(procMu.Unlock)
}
