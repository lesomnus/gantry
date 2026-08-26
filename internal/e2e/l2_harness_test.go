//go:build e2e

// The L2 real-daemon tier (docs/e2e-testing.md): the same in-process gantry, but
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
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// l2opt configures the L2 topology. The defaults reproduce the original
// harness — a writable cache, no route declared, stock worker knobs — so a test
// that names no option gets exactly what TestL2CopyAndEnginePull always got.
type l2opt func(*l2cfg)

type l2cfg struct {
	remoteCache   string              // stores.remote.cache
	routes        []config.CacheRoute // stores.remote.caches (scoped form)
	readOnlyCache bool                // the cache registry refuses writes
	farStore      bool                // declare a third registry store, `far`
	worker        config.WorkerConfig
	throttle      int                    // bytes/sec ceiling in front of the origin; 0 disables
	retention     []config.RetentionRule // retention rules for `edge`
}

// l2WithRemoteCache declares a store as the origin's cache, so copies that read
// the origin may be routed through it.
func l2WithRemoteCache(store string) l2opt {
	return func(c *l2cfg) { c.remoteCache = store }
}

// l2WithRoutes is the scoped form of l2WithRemoteCache.
func l2WithRoutes(routes ...config.CacheRoute) l2opt {
	return func(c *l2cfg) { c.routes = routes }
}

// l2WithReadOnlyCache starts the cache registry in read-only mode, so a fill of
// it fails against a real registry's own refusal rather than a fake error.
func l2WithReadOnlyCache() l2opt { return func(c *l2cfg) { c.readOnlyCache = true } }

// l2WithFarStore adds a third registry store named `far`, for jobs that deliver
// to a registry rather than to the daemon.
func l2WithFarStore() l2opt { return func(c *l2cfg) { c.farStore = true } }

func l2WithWorker(w config.WorkerConfig) l2opt { return func(c *l2cfg) { c.worker = w } }

// l2WithThrottledOrigin puts a bandwidth-limited proxy in front of the origin
// registry and points the `remote` store at it, so a fill out of the origin
// takes long enough for a second job to find it in flight.
func l2WithThrottledOrigin(bytesPerSec int) l2opt {
	return func(c *l2cfg) { c.throttle = bytesPerSec }
}

// l2WithRetention turns on the retention inventory for `edge` with these rules,
// so what the daemon holds after a job can be asked about by repository. A rule
// pattern may use the placeholders `{remote}` and `{cache}` for the registry
// hosts, which are only known once their containers have a published port.
func l2WithRetention(rules ...config.RetentionRule) l2opt {
	return func(c *l2cfg) { c.retention = rules }
}

type l2harness struct {
	t             *testing.T
	client        pb.Client
	cli           *client.Client
	remote, cache string // 127.0.0.1:<port>
	far           string // 127.0.0.1:<port>, only with l2WithFarStore
	originHost    string // the origin registry itself, unthrottled (seeding)

	remoteID, cacheID, farID string // container ids, for staging an outage
}

func newL2Harness(t *testing.T, opts ...l2opt) *l2harness {
	t.Helper()
	var lc l2cfg
	for _, o := range opts {
		o(&lc)
	}
	cli := dockerClientOrSkip(t)
	daemonHost, needFwd := remoteDaemon()

	h := &l2harness{t: t, cli: cli}
	h.remote, h.remoteID = startRegistryContainerCfg(t, cli, daemonHost, needFwd, "")
	h.originHost = h.remote
	if lc.throttle > 0 {
		h.remote = startThrottledProxy(t, h.originHost, lc.throttle)
	}
	cacheCfg := ""
	if lc.readOnlyCache {
		cacheCfg = readOnlyRegistryConfig
	}
	h.cache, h.cacheID = startRegistryContainerCfg(t, cli, daemonHost, needFwd, cacheCfg)
	if lc.readOnlyCache {
		requireWritesRefused(t, h.cache)
	}

	edge := config.StoreConfig{Kind: "docker", Address: dockerAddr()}
	if lc.retention != nil {
		rules := make([]config.RetentionRule, len(lc.retention))
		for i, r := range lc.retention {
			r.Repo = strings.NewReplacer("{remote}", h.remote, "{cache}", h.cache).Replace(r.Repo)
			rules[i] = r
		}
		edge.Retention = &config.StoreRetention{
			Path:  filepath.Join(t.TempDir(), "edge.db"),
			Rules: rules,
		}
	}
	stores := map[string]config.StoreConfig{
		"remote": {Kind: "oci", Host: h.remote, Insecure: true, Cache: lc.remoteCache, Caches: lc.routes},
		"cache":  {Kind: "oci", Host: h.cache, Insecure: true, Mode: "copy"},
		"edge":   edge,
	}
	if lc.farStore {
		h.far, h.farID = startRegistryContainerCfg(t, cli, daemonHost, needFwd, "")
		stores["far"] = config.StoreConfig{Kind: "oci", Host: h.far, Insecure: true, Mode: "copy"}
	}
	cfg := &config.Config{Stores: stores, Worker: lc.worker}
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

// startRegistryContainer runs a registry container (GANTRY_E2E_REGISTRY, default
// registry:2), publishes it on the daemon host's 127.0.0.1:<ephemeral>, and (in
// the separate-netns case) forwards the same port from the test process. Returns
// 127.0.0.1:<port>.
func startRegistryContainer(t *testing.T, cli *client.Client, daemonHost string, needFwd bool) string {
	addr, _ := startRegistryContainerCfg(t, cli, daemonHost, needFwd, "")
	return addr
}

// registryConfigPath reports where this registry image expects its config, as a
// tar-relative path. The entrypoint is handed that path as its argument, so the
// image itself is the authority: distribution moved it between majors (2.x
// /etc/docker/registry/config.yml, 3.x /etc/distribution/config.yml) and writing
// to the wrong one is SILENT — the registry starts on its own writable default,
// and a test that meant to stage a refusal stages nothing at all.
func registryConfigPath(t *testing.T, cli *client.Client, regImage string) string {
	t.Helper()
	info, err := cli.ImageInspect(context.Background(), regImage)
	if err != nil {
		t.Fatalf("inspect registry image %q: %v", regImage, err)
	}
	for _, arg := range info.Config.Cmd {
		if strings.HasSuffix(arg, ".yml") || strings.HasSuffix(arg, ".yaml") {
			return strings.TrimPrefix(arg, "/")
		}
	}
	t.Fatalf("registry image %q names no config file in its cmd %v; "+
		"the read-only config would land somewhere it is never read", regImage, info.Config.Cmd)
	return ""
}

// requireWritesRefused fails the test unless the registry refuses an upload.
// Staging a refusal is only worth anything if it took: without this a config
// that lands in the wrong place, or a schema that moved, turns every test built
// on it into one that quietly proves nothing.
func requireWritesRefused(t *testing.T, addr string) {
	t.Helper()
	res, err := http.Post("http://"+addr+"/v2/probe/readonly/blobs/uploads/", "", nil)
	if err != nil {
		t.Fatalf("probe %s for writability: %v", addr, err)
	}
	defer res.Body.Close()
	if res.StatusCode < 400 {
		t.Fatalf("the cache registry accepted an upload (HTTP %d); it was meant to be read-only, "+
			"so nothing built on that refusal would be proving anything", res.StatusCode)
	}
}

// readOnlyRegistryConfig is the stock config with writes disabled, so a push is
// refused by a registry that is otherwise perfectly alive and answers every
// read. The schema is common to distribution 2.x and 3.x; only the path it must
// be written to differs, which registryConfigPath derives from the image.
const readOnlyRegistryConfig = `version: 0.1
log:
  fields:
    service: registry
storage:
  cache:
    blobdescriptor: inmemory
  filesystem:
    rootdirectory: /var/lib/registry
  maintenance:
    readonly:
      enabled: true
http:
  addr: :5000
  headers:
    X-Content-Type-Options: [nosniff]
`

// injectFile writes one file into a created (not yet started) container.
func injectFile(t *testing.T, cli *client.Client, id, path, content string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: path, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cli.CopyToContainer(context.Background(), id, "/", &buf, container.CopyToContainerOptions{}); err != nil {
		t.Fatalf("inject %s: %v", path, err)
	}
}

// startRegistryContainerCfg is startRegistryContainer with a replacement
// registry config and the container id, which a test that stages an outage
// needs. An empty cfgYAML keeps the image's own config.
//
// The config file rather than REGISTRY_* environment: distribution env
// overrides REPLACE the `storage` map instead of merging into it, so setting
// only the maintenance key leaves the registry with no storage driver and it
// exits at startup — a registry that is gone, not one that refuses writes.
func startRegistryContainerCfg(t *testing.T, cli *client.Client, daemonHost string, needFwd bool, cfgYAML string) (addr, id string) {
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
		}, nil, nil, "")
	if err != nil {
		t.Skipf("create registry %q (is the image present?): %v", regImage, err)
	}
	if cfgYAML != "" {
		injectFile(t, cli, resp.ID, registryConfigPath(t, cli, regImage), cfgYAML)
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
	addr = "127.0.0.1:" + port
	if needFwd {
		startForward(t, port, daemonHost+":"+port)
	}
	waitRegistry(t, addr)
	return addr, resp.ID
}

// kill stops a registry container and waits until its published port stops
// answering, so a test can stage a registry outage and know it has happened.
func (h *l2harness) kill(id, addr string) {
	h.t.Helper()
	if err := h.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true}); err != nil {
		h.t.Fatalf("kill registry %s: %v", addr, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return
		}
		c.Close()
		if time.Now().After(deadline) {
			h.t.Fatalf("registry %s still answering after it was removed", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startThrottledProxy forwards 127.0.0.1:<free> to target at roughly rate
// bytes/sec per direction, so a copy through it takes a predictable few seconds
// instead of finishing before another job can observe it.
func startThrottledProxy(t *testing.T, target string, rate int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("throttle listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	// One tick of budget, ten times a second: small enough that a slow transfer
	// is paced smoothly rather than in visible bursts.
	const ticksPerSec = 10
	chunk := rate / ticksPerSec
	if chunk < 1 {
		chunk = 1
	}
	paced := func(dst io.Writer, src io.Reader) {
		buf := make([]byte, chunk)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
				time.Sleep(time.Second / ticksPerSec)
			}
			if err != nil {
				return
			}
		}
	}
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
				go paced(u, c)
				paced(c, u)
			}(c)
		}
	}()
	return l.Addr().String()
}

// dockerClientOrSkip returns a docker client, skipping the test when no daemon
// is reachable.
func dockerClientOrSkip(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.WithHost(dockerAddr()), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("no reachable docker daemon (%s): %v", dockerAddr(), err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
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

// waitRegistry blocks until the registry ANSWERS, not merely until something
// accepts a connection. Docker publishes the host port as soon as the container
// is created, so a TCP dial succeeds while the registry process inside is still
// starting — the next request then dies on a reset. Any HTTP status from /v2/
// means the registry itself is up, which is the thing being waited for.
func waitRegistry(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	cl := &http.Client{Timeout: 2 * time.Second}
	for {
		res, err := cl.Get("http://" + addr + "/v2/")
		if err == nil {
			res.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry %s never answered: %v", addr, err)
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
