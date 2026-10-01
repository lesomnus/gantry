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
	"github.com/lesomnus/gantry/internal/dockertest"
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
	dockertest.Lock(t)

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
	// on_unavailable=kill makes the "trusted verdict is allowed" assertion
	// load-bearing: with the verifier stubbed unavailable, a container survives
	// ONLY if the trusted cache path decided it — a broken cache path would fall
	// through to kill.
	m := NewManager([]Store{{Name: "dockerd", Engine: eng}}, cache, unavailableVerifier{}, nil, Options{OnUnavailable: "kill"})

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
		// THE WATCHER DOES NOT WAIT FOR A START EVENT. It reconciles every
		// container already running on the daemon, and with the verifier stubbed
		// unavailable and on_unavailable=kill, every one of them is an image
		// with no trusted verdict: removed, and its image after it. On a CI
		// runner the daemon is this test's alone and that is nothing. On a
		// workstation it was eleven devcontainers and the sessions inside them
		// (#41) — while this subtest FAILED, because the watcher spent its
		// thirty seconds on them.
		//
		// So the daemon has to be this test's before the watcher starts. A
		// refusal, not a filter: the reconcile is what enforcement is, and a
		// test that taught it to look away would be testing something else.
		cs, err := cli.ContainerList(ctx, container.ListOptions{})
		if err != nil {
			t.Fatalf("list running containers: %v", err)
		}
		var names []string
		for _, c := range cs {
			names = append(names, c.Names...)
		}
		if others := bystanders(names); len(others) > 0 {
			t.Skipf("the daemon runs %d container(s) this test did not start (%s); the watcher would quarantine "+
				"every one of them. Run it on a daemon of its own: the devcontainer's, or one named by DOCKER_HOST",
				len(others), strings.Join(others, ", "))
		}

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

// ownPrefix names the containers this test starts (see `run` above).
const ownPrefix = "/gantry-e2e-"

// bystanders are the running containers this test did not start: on any daemon
// but one of the test's own, somebody's work.
func bystanders(names []string) []string {
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n, ownPrefix) {
			out = append(out, strings.TrimPrefix(n, "/"))
		}
	}
	return out
}

// The refusal above rests on telling this test's containers from anybody
// else's, so that is pinned without a daemon. A container merely NAMED like
// gantry — the service itself, on a robot or in a harness — is a bystander.
func TestBystanders(t *testing.T) {
	got := bystanders([]string{
		"/gantry-e2e-allow",
		"/hday-os_devcontainer-dev-1",
		"/gantry-e2e-watch",
		"/gantry",
		"/gantry-mgr-test",
		"/cld",
	})
	want := []string{"hday-os_devcontainer-dev-1", "gantry", "gantry-mgr-test", "cld"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("bystanders = %v, want %v", got, want)
	}
	if b := bystanders([]string{"/gantry-e2e-allow", "/gantry-e2e-watch"}); len(b) != 0 {
		t.Errorf("the test's own containers were taken for bystanders: %v", b)
	}
}
