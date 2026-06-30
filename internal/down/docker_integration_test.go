package down

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
)

func dockerAddr() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "unix:///var/run/docker.sock"
}

// TestDockerTargetLive exercises the real Docker client against a reachable
// daemon. It skips when none is available so the suite stays hermetic by default.
func TestDockerTargetLive(t *testing.T) {
	tgt, err := newDockerTarget(config.TargetConfig{Name: "live", Kind: "docker", Address: dockerAddr()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer tgt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := tgt.Ready(ctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}

	if err := tgt.Pull(ctx, "hello-world:latest"); err != nil {
		t.Fatalf("pull: %v", err)
	}
	// A bad reference must surface as an error from the JSONMessage stream.
	if err := tgt.Pull(ctx, "library/gantry-does-not-exist:nope"); err == nil {
		t.Error("expected error pulling a nonexistent image")
	}
}
