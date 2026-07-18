package enforce

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/verify"
)

type nopSink struct{}

func (nopSink) Layer(down.LayerUpdate) {}

func dockerAddr() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "unix:///var/run/docker.sock"
}

// unavailableVerifier stands in for the live verifier so the ONLY decisive input
// is the seeded cache. Live notation verification is exercised by the verify
// package's integration and local-layout tests; here we drive the docker + cache
// + kill pipeline against a real daemon.
type unavailableVerifier struct{}

func (unavailableVerifier) Verify(context.Context, config.StoreConfig, name.Reference) (verify.Result, error) {
	return verify.Result{}, errors.New("registry unreachable (e2e stub)")
}
func (unavailableVerifier) Describe() verify.Description        { return verify.Description{} }
func (unavailableVerifier) Reload() (verify.Description, error) { return verify.Description{}, nil }

// TestEnforceDockerE2E drives the enforce.Manager against a real docker daemon:
// a container whose image digest has a trusted verdict is left running, and one
// with an untrusted verdict is quarantined — both by a direct decision and via
// the live event watcher.
func TestEnforceDockerE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("no reachable docker daemon: %v", err)
	}

	engIface, err := down.New(config.StoreConfig{Name: "dockerd", Kind: "docker", Address: dockerAddr()})
	if err != nil {
		t.Fatalf("down.New: %v", err)
	}
	defer engIface.Close()
	eng := engIface.(Engine)

	const ref = "alpine:latest"
	ensureImage := func(t *testing.T) string {
		t.Helper()
		if _, err := engIface.Pull(ctx, ref, "", "", nil, nil, nopSink{}); err != nil {
			t.Fatalf("pull %s: %v", ref, err)
		}
		img, err := cli.ImageInspect(ctx, ref)
		if err != nil || len(img.RepoDigests) == 0 {
			t.Fatalf("inspect %s: %v (repoDigests=%v)", ref, err, img.RepoDigests)
		}
		rd := img.RepoDigests[0]
		return rd[strings.LastIndex(rd, "@")+1:]
	}

	cache, err := verify.OpenCache(filepath.Join(t.TempDir(), "v.db"), 28*24*time.Hour, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	m := NewManager([]Store{{Name: "dockerd", Engine: eng}}, cache, unavailableVerifier{}, nil, Options{OnUnavailable: "grace"})

	run := func(t *testing.T, name string) string {
		t.Helper()
		created, err := cli.ContainerCreate(ctx,
			&container.Config{Image: ref, Cmd: []string{"sleep", "120"}},
			&container.HostConfig{}, nil, nil, name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			_ = cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
		})
		if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		return created.ID
	}
	gone := func(id string) bool {
		_, err := cli.ContainerInspect(ctx, id)
		return dockerclient.IsErrNotFound(err)
	}

	t.Run("trusted verdict is allowed", func(t *testing.T) {
		digest := ensureImage(t)
		if err := cache.Put(digest, true, config.VerifyRequire, ""); err != nil {
			t.Fatal(err)
		}
		id := run(t, "gantry-e2e-allow")
		m.handle(ctx, eng, down.StartEvent{ContainerID: id, Image: ref})
		if gone(id) {
			t.Error("a container with a trusted verdict must not be quarantined")
		}
	})

	t.Run("untrusted verdict is quarantined (direct)", func(t *testing.T) {
		digest := ensureImage(t)
		if err := cache.Put(digest, false, config.VerifyRequire, ""); err != nil {
			t.Fatal(err)
		}
		id := run(t, "gantry-e2e-kill")
		m.handle(ctx, eng, down.StartEvent{ContainerID: id, Image: ref})
		if !gone(id) {
			t.Error("a container with an untrusted verdict must be quarantined")
		}
	})

	t.Run("untrusted verdict is quarantined by the watcher", func(t *testing.T) {
		digest := ensureImage(t)
		if err := cache.Put(digest, false, config.VerifyRequire, ""); err != nil {
			t.Fatal(err)
		}
		wctx, wcancel := context.WithCancel(ctx)
		m.StartWatchers(wctx)
		defer func() { wcancel(); m.Stop() }() // cancel first, THEN join
		time.Sleep(500 * time.Millisecond)     // let the events subscription attach

		id := run(t, "gantry-e2e-watch")
		deadline := time.Now().Add(30 * time.Second)
		for !gone(id) {
			if time.Now().After(deadline) {
				t.Fatal("the watcher did not quarantine the untrusted container within timeout")
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
}
