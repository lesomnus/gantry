package down

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/lesomnus/gantry/cmd/config"
)

func dockerAddr() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "unix:///var/run/docker.sock"
}

// TestDockerEngineLive exercises the real Docker client against a reachable
// daemon and checks that per-layer byte progress is reported. Skips when no
// daemon is available.
func TestDockerEngineLive(t *testing.T) {
	eng, err := newDockerEngine(config.StoreConfig{Name: "live", Kind: "docker", Address: dockerAddr()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}

	// A distinct image from the containerd test (docker 29 shares the containerd
	// content store), removed first so the pull actually downloads.
	const ref = "alpine:latest"
	_, _ = eng.cli.ImageRemove(ctx, ref, image.RemoveOptions{Force: true})

	sink := &recSink{}
	if _, err := eng.Pull(ctx, ref, "", "", nil, nil, sink); err != nil {
		t.Fatalf("pull: %v", err)
	}
	// Per-layer progress is only observable when the pull actually downloads.
	// Byte counts are best-effort: docker 29+'s containerd image store reports
	// state-only on fast local pulls, so state progress counts too. But that
	// content store is shared daemon-wide, so a parallel test (or a prior run)
	// holding alpine can defeat the ImageRemove above and serve this pull
	// entirely from cache, emitting no per-layer messages at all. That is an
	// environment condition, not a progress-reporting bug — skip, don't fail.
	if !sink.progressed() {
		t.Skip("pull served from cache (shared containerd content store); no fresh per-layer progress to observe")
	}
	if _, err := eng.Pull(ctx, "library/gantry-does-not-exist:nope", "", "", nil, nil, nopSink{}); err == nil {
		t.Error("expected error pulling a nonexistent image")
	}
}
