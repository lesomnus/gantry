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
	if err := eng.Pull(ctx, ref, "", sink); err != nil {
		t.Fatalf("pull: %v", err)
	}
	// Per-layer progress must be observed. Byte counts are best-effort: docker
	// 29+'s containerd image store reports state-only on fast local pulls, so
	// accept state progress when no bytes stream.
	if !sink.progressed() {
		t.Error("expected per-layer progress (bytes or state) from the pull stream")
	}
	if err := eng.Pull(ctx, "library/gantry-does-not-exist:nope", "", nopSink{}); err == nil {
		t.Error("expected error pulling a nonexistent image")
	}
}
