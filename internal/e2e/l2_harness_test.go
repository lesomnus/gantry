//go:build e2e

// The L2 real-daemon tier (docs/e2e-plan.md): the same in-process gantry, but
// backed by real registry:2 containers and the real docker daemon. Build-tagged
// `e2e` so it stays out of the default `go test`; self-skips when no docker
// daemon is reachable.
//
// Loopback model: registries are published on the daemon host's 127.0.0.1:<port>
// (docker auto-trusts 127.0.0.0/8 as insecure, so no daemon.json). In CI the test
// process and the daemon share a network namespace, so gantry uses the same
// 127.0.0.1:<port>. In the devcontainer they do not, so a same-address forwarder
// (127.0.0.1:<port> → docker:<port>) is started when DOCKER_HOST is a remote tcp
// endpoint.
package e2e

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/app"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func dockerAddr() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	return "unix:///var/run/docker.sock"
}

// remoteDaemon reports whether the daemon is in a different network namespace
// than the test process (a tcp:// host that is not loopback), so registries it
// publishes need a same-address forwarder.
func remoteDaemon() (host string, remote bool) {
	h := dockerAddr()
	if !strings.HasPrefix(h, "tcp://") {
		return "", false
	}
	hp := strings.TrimPrefix(h, "tcp://")
	host, _, _ = net.SplitHostPort(hp)
	return host, host != "127.0.0.1" && host != "localhost"
}

type l2harness struct {
	t             *testing.T
	client        pb.Client
	cli           *client.Client
	remote, cache string // 127.0.0.1:<port>
}

func newL2Harness(t *testing.T) *l2harness {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost(dockerAddr()), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pcancel()
	if _, err := cli.Ping(pctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}
	t.Cleanup(func() { cli.Close() })

	daemonHost, needFwd := remoteDaemon()

	h := &l2harness{t: t, cli: cli}
	h.remote = h.startRegistry(daemonHost, needFwd)
	h.cache = h.startRegistry(daemonHost, needFwd)

	cfg := &config.Config{
		Stores: map[string]config.StoreConfig{
			"remote": {Kind: "oci", Host: h.remote, Insecure: true},
			"cache":  {Kind: "oci", Host: h.cache, Insecure: true, Mode: "copy"},
			"edge":   {Kind: "docker", Address: dockerAddr()},
		},
	}
	if err := cfg.Evaluate(); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := app.Build(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatalf("build: %v", err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.GRPC.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.GRPC.Stop()
		cancel()
		srv.Stop()
		_ = lis.Close()
	})
	h.client = pb.NewClient(conn)
	return h
}

// startRegistry runs a registry:2 container, publishes it on the daemon host's
// 127.0.0.1:<ephemeral>, and (in the separate-netns case) forwards the same port
// from the test process. Returns 127.0.0.1:<port>.
func (h *l2harness) startRegistry(daemonHost string, needFwd bool) string {
	h.t.Helper()
	ctx := context.Background()
	resp, err := h.cli.ContainerCreate(ctx,
		&container.Config{Image: "registry:2", ExposedPorts: nat.PortSet{"5000/tcp": {}}},
		&container.HostConfig{
			AutoRemove:   true,
			PortBindings: nat.PortMap{"5000/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "0"}}},
		}, nil, nil, "")
	if err != nil {
		h.t.Skipf("create registry (is the registry:2 image present?): %v", err)
	}
	if err := h.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		h.t.Fatalf("start registry: %v", err)
	}
	h.t.Cleanup(func() {
		_ = h.cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	})
	info, err := h.cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		h.t.Fatalf("inspect registry: %v", err)
	}
	port := info.NetworkSettings.Ports["5000/tcp"][0].HostPort
	addr := "127.0.0.1:" + port
	if needFwd {
		startForward(h.t, port, daemonHost+":"+port)
	}
	// Wait for /v2/ to answer through whatever path gantry will use.
	waitRegistry(h.t, addr)
	return addr
}

// startForward proxies 127.0.0.1:<port> (test process) to target (the daemon
// host), so gantry reaches a daemon-published registry at the same reference the
// daemon uses.
func startForward(t *testing.T, port, target string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("forward listen %s: %v", port, err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				u, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer u.Close()
				go io.Copy(u, c)
				io.Copy(c, u)
			}(c)
		}
	}()
}

func waitRegistry(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry %s never came up: %v", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *l2harness) add(req *pb.JobAddRequest) *pb.Job {
	h.t.Helper()
	job, err := h.client.Job().Add(context.Background(), req)
	if err != nil {
		h.t.Fatalf("job add: %v", err)
	}
	return job
}

func (h *l2harness) waitDone(id string) *pb.Job {
	h.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		job, err := h.client.Job().Get(context.Background(), pb.JobGetById(id))
		if err != nil {
			h.t.Fatalf("job get: %v", err)
		}
		switch job.GetState() {
		case pb.JobState_JOB_STATE_DONE, pb.JobState_JOB_STATE_FAILED, pb.JobState_JOB_STATE_CANCELED:
			return job
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("job %s did not terminate; state %v", id, job.GetState())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// removeImage best-effort deletes a ref from the daemon so a pull actually
// downloads.
func (h *l2harness) removeImage(ref string) {
	_, _ = h.cli.ImageRemove(context.Background(), ref, image.RemoveOptions{Force: true})
}
