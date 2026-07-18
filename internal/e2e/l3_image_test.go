//go:build e2e

// The L3 image tier (docs/e2e-testing.md): drive the actual shipped container
// image — the `FROM scratch` multi-stage build artifact named by
// GANTRY_E2E_IMAGE — rather than a `go build` binary. It is the only tier that
// exercises what the image alone can break: the static binary running on an
// empty base, the non-root user (65532) creating the bbolt audit log, the baked
// entrypoint, and end-to-end serving. Self-skips unless GANTRY_E2E_IMAGE names a
// loaded image.
//
// Networking: gantry and two registry containers share a user-defined network so
// gantry reaches the registries by DNS alias (`remote:5000` / `cache:5000`),
// independent of the daemon's loopback trust. The registries also publish to the
// daemon host's loopback so the test process can seed and assert; in the
// separate-netns devcontainer a same-address forwarder bridges those, exactly as
// the L2 tier does.
package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"

	"github.com/docker/docker/client"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// nonrootUID matches the `USER 65532:65532` in the Dockerfile's app stage.
const nonrootUID = 65532

func TestL3Image(t *testing.T) {
	imageRef := os.Getenv("GANTRY_E2E_IMAGE")
	if imageRef == "" {
		t.Skip("GANTRY_E2E_IMAGE unset; set it to a built gantry image to run the image tier")
	}
	cli := dockerClientOrSkip(t)
	daemonHost, needFwd := remoteDaemon()

	// One user network the whole cast shares; gantry resolves the registries on it
	// by alias.
	netName := fmt.Sprintf("gantry-e2e-%d", time.Now().UnixNano())
	netRes, err := cli.NetworkCreate(context.Background(), netName, network.CreateOptions{Driver: "bridge"})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() { _ = cli.NetworkRemove(context.Background(), netRes.ID) })

	remoteReg := imgRegistry(t, cli, netName, "remote", daemonHost, needFwd)
	cacheReg := imgRegistry(t, cli, netName, "cache", daemonHost, needFwd)
	seedImage(t, remoteReg, "lib/app", "1")

	// gantry talks to the registries by network alias over plain HTTP; the audit
	// log lands in /data, which the injected tar owns as the non-root uid so the
	// scratch user can create the bbolt file.
	cfg := `serve:
  addr: "0.0.0.0:7000"
  events:
    path: "/data/events.db"
stores:
  remote: { kind: "oci", host: "remote:5000", insecure: true }
  cache: { kind: "oci", host: "cache:5000", insecure: true, mode: "copy" }
`
	gc := imgRunGantry(t, cli, imageRef, netName, cfg, daemonHost, needFwd)

	// A real copy across two registries: proves the shipped binary serves and
	// moves blobs end to end.
	job, err := gc.Job().Add(context.Background(), copyReq("remote", "cache"))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	waitTerminal(t, gc, job.GetId())

	if !hasTag(t, cacheReg, "lib/app", "1") {
		t.Fatal("cache registry is missing lib/app:1 after the copy")
	}
	// A recorded event proves the non-root user actually wrote the bbolt log at
	// /data/events.db — a permission the plain binary tiers never exercise.
	if got := listEvents(t, gc); got == 0 {
		t.Fatal("no audit events; the non-root user could not write /data/events.db")
	}
}

// imgRegistry runs a registry container attached to netName under `alias`, and
// also publishes it on the daemon host's loopback (plus a forwarder in the
// separate-netns case) so the test process can seed and inspect it. Returns the
// test-reachable 127.0.0.1:<port>.
func imgRegistry(t *testing.T, cli *client.Client, netName, alias, daemonHost string, needFwd bool) string {
	t.Helper()
	ctx := context.Background()
	regImage := os.Getenv("GANTRY_E2E_REGISTRY")
	if regImage == "" {
		regImage = "registry:2"
	}
	resp, err := cli.ContainerCreate(ctx,
		&container.Config{Image: regImage, ExposedPorts: nat.PortSet{"5000/tcp": {}}},
		&container.HostConfig{
			AutoRemove:   true,
			PortBindings: nat.PortMap{"5000/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "0"}}},
		},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {Aliases: []string{alias}},
		}}, nil, "")
	if err != nil {
		t.Skipf("create registry %q (is the image present?): %v", regImage, err)
	}
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start registry: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})
	info, err := cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		t.Fatalf("inspect registry: %v", err)
	}
	port := info.NetworkSettings.Ports["5000/tcp"][0].HostPort
	addr := "127.0.0.1:" + port
	if needFwd {
		startForward(t, port, daemonHost+":"+port)
	}
	waitRegistry(t, addr)
	return addr
}

// imgRunGantry creates the gantry image container on netName, injects the config
// and a non-root-owned /data before start (so nothing depends on a bind mount
// reaching a possibly-remote daemon), publishes the gRPC port, and returns a
// ready client.
func imgRunGantry(t *testing.T, cli *client.Client, imageRef, netName, cfg, daemonHost string, needFwd bool) pb.Client {
	t.Helper()
	ctx := context.Background()
	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:        imageRef,
			Cmd:          []string{"--config", "/gantry.yaml", "serve"},
			ExposedPorts: nat.PortSet{"7000/tcp": {}},
		},
		&container.HostConfig{
			PortBindings: nat.PortMap{"7000/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "0"}}},
		},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {},
		}}, nil, "")
	if err != nil {
		t.Fatalf("create gantry: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if r, e := cli.ContainerLogs(context.Background(), resp.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true}); e == nil {
				b, _ := io.ReadAll(r)
				r.Close()
				t.Logf("gantry container logs:\n%s", b)
			}
		}
		_ = cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})

	injectGantryFiles(t, cli, resp.ID, cfg)

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start gantry: %v", err)
	}
	info, err := cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		t.Fatalf("inspect gantry: %v", err)
	}
	port := info.NetworkSettings.Ports["7000/tcp"][0].HostPort
	if needFwd {
		startForward(t, port, daemonHost+":"+port)
	}
	addr := "127.0.0.1:" + port

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	hc := grpc_health_v1.NewHealthClient(conn)
	deadline := time.Now().Add(30 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := hc.Check(cctx, &grpc_health_v1.HealthCheckRequest{})
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gantry image never became reachable at %s: %v", addr, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return pb.NewClient(conn)
}

// injectGantryFiles copies the config file and a non-root-owned /data directory
// into the created (not-yet-started) container. Doing it via the container copy
// API rather than a bind mount keeps it correct against a remote daemon.
func injectGantryFiles(t *testing.T, cli *client.Client, id, cfg string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: "gantry.yaml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(cfg)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: nonrootUID, Gid: nonrootUID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cli.CopyToContainer(context.Background(), id, "/", &buf, container.CopyToContainerOptions{CopyUIDGID: true}); err != nil {
		t.Fatalf("inject files: %v", err)
	}
}
