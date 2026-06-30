package down

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
)

func containerdAddr() string {
	if a := os.Getenv("CONTAINERD_ADDRESS"); a != "" {
		return a
	}
	return "/run/docker/containerd/containerd.sock"
}

// TestContainerdEngineLive exercises the real containerd client. It skips when
// no socket is reachable. docker's bundled containerd keeps images in the "moby"
// namespace.
func TestContainerdEngineLive(t *testing.T) {
	if _, err := os.Stat(containerdAddr()); err != nil {
		t.Skipf("no containerd socket at %s", containerdAddr())
	}
	eng, err := newContainerdEngine(config.StoreConfig{
		Name:      "live",
		Kind:      "containerd",
		Address:   containerdAddr(),
		Namespace: "moby",
	})
	if err != nil {
		t.Skipf("containerd client (%s): %v", containerdAddr(), err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable containerd (%s): %v", containerdAddr(), err)
	}

	if err := eng.Pull(ctx, "docker.io/library/busybox:latest", &recSink{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}
