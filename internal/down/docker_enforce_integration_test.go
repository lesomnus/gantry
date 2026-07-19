package down

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/lesomnus/gantry/cmd/config"
)

// TestDockerEnforcerLive exercises the Enforcer capability (WatchStarts,
// ListRunning, ResolveImage, RemoveContainer) against a real docker daemon.
// Skips when no daemon is reachable.
func TestDockerEnforcerLive(t *testing.T) {
	eng, err := newDockerEngine(config.StoreConfig{Name: "live", Kind: "docker", Address: dockerAddr()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer eng.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := eng.Ready(ctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}

	const ref = "alpine:latest"
	if _, err := eng.Pull(ctx, ref, "", "", nil, nil, nopSink{}); err != nil {
		t.Fatalf("pull %s: %v", ref, err)
	}

	// runContainer creates and starts a long-lived container, returning its id;
	// it force-removes on cleanup.
	runContainer := func(t *testing.T, name string) string {
		t.Helper()
		created, err := eng.cli.ContainerCreate(ctx,
			&container.Config{Image: ref, Cmd: []string{"sleep", "120"}},
			&container.HostConfig{}, nil, nil, name)
		if err != nil {
			t.Fatalf("container create: %v", err)
		}
		t.Cleanup(func() {
			_ = eng.cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
		})
		if err := eng.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			t.Fatalf("container start: %v", err)
		}
		return created.ID
	}

	t.Run("ListRunning and ResolveImage and RemoveContainer", func(t *testing.T) {
		id := runContainer(t, "gantry-enforce-test-1")

		running, err := eng.ListRunning(ctx)
		if err != nil {
			t.Fatalf("ListRunning: %v", err)
		}
		if !slices.ContainsFunc(running, func(s StartEvent) bool { return s.ContainerID == id }) {
			t.Fatalf("ListRunning did not include %s", id)
		}

		ci, err := eng.ResolveImage(ctx, id)
		if err != nil {
			t.Fatalf("ResolveImage: %v", err)
		}
		if ci.ImageID == "" {
			t.Error("ResolveImage returned empty ImageID")
		}
		if len(ci.RepoDigests) == 0 && ci.ManifestDigest == "" {
			t.Errorf("ResolveImage found no content digest (RepoDigests and ManifestDigest both empty): %+v", ci)
		}
		t.Logf("resolved: imageID=%s repoDigests=%v manifestDigest=%s", ci.ImageID, ci.RepoDigests, ci.ManifestDigest)

		if err := eng.RemoveContainer(ctx, id, true); err != nil {
			t.Fatalf("RemoveContainer(force): %v", err)
		}
		running, _ = eng.ListRunning(ctx)
		if slices.ContainsFunc(running, func(s StartEvent) bool { return s.ContainerID == id }) {
			t.Errorf("container %s still running after force remove", id)
		}
		// removing an already-gone container is success
		if err := eng.RemoveContainer(ctx, id, true); err != nil {
			t.Errorf("RemoveContainer on a gone container should be nil, got %v", err)
		}
	})

	t.Run("WatchStarts observes a start event", func(t *testing.T) {
		wctx, wcancel := context.WithCancel(ctx)
		defer wcancel()
		events := make(chan StartEvent, 16)
		go func() { _ = eng.WatchStarts(wctx, func(e StartEvent) { events <- e }) }()
		// give the events subscription a moment to attach before we start.
		time.Sleep(300 * time.Millisecond)

		id := runContainer(t, "gantry-enforce-test-2")

		deadline := time.After(15 * time.Second)
		for {
			select {
			case e := <-events:
				if e.ContainerID == id {
					if e.Image == "" {
						t.Errorf("start event for %s carried no image", id)
					}
					return
				}
			case <-deadline:
				t.Fatalf("did not observe a start event for %s within timeout", id)
			}
		}
	})
}
