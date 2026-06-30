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

// TestContainerdTargetLive exercises the real containerd client. It skips when
// no socket is reachable (e.g. before the dev container shares docker's bundled
// containerd). docker's containerd keeps images in the "moby" namespace.
func TestContainerdTargetLive(t *testing.T) {
	if _, err := os.Stat(containerdAddr()); err != nil {
		t.Skipf("no containerd socket at %s", containerdAddr())
	}
	tgt, err := newContainerdTarget(config.TargetConfig{
		Name:      "live",
		Kind:      "containerd",
		Address:   containerdAddr(),
		Namespace: "moby",
	})
	if err != nil {
		t.Skipf("containerd client (%s): %v", containerdAddr(), err)
	}
	defer tgt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := tgt.Ready(ctx); err != nil {
		t.Skipf("no reachable containerd (%s): %v", containerdAddr(), err)
	}

	if err := tgt.Pull(ctx, "docker.io/library/hello-world:latest"); err != nil {
		t.Fatalf("pull: %v", err)
	}
}
