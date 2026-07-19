// Package e2e is the hermetic end-to-end suite (docs/e2e-testing.md, L1): it stands
// up the real gantry gRPC server in-process (internal/app.Build over bufconn),
// backs the source and cache stores with in-memory OCI registries, uses a fake
// engine daemon, and injects a clock so time-dependent GC is deterministic — all
// in plain `go test`, no external infrastructure.
package e2e

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/app"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// clock is a manually-advanced fake clock for deterministic time-based GC.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t      *testing.T
	client pb.Client
	conn   *grpc.ClientConn

	engine *fakeEngine
	clock  *clock

	remote, cache string // registry host:port
	cacheUploads  *int32
}

type harnessOpt func(*harnessCfg)

type harnessCfg struct {
	cacheMode   string // "copy" (default) | "proxy"
	tlsCache    bool   // serve the cache over HTTPS with a private CA (ca_cert)
	rules       []config.RetentionRule
	rewrite     []config.RewriteRule
	verify      *config.VerifyConfig
	storeVerify map[string]*config.StoreVerify
	enforce     bool // enable serve.enforce (quarantine) + a verdict cache on `edge`
}

func withCacheMode(m string) harnessOpt { return func(c *harnessCfg) { c.cacheMode = m } }
func withTLSCache() harnessOpt          { return func(c *harnessCfg) { c.tlsCache = true } }
func withRules(r ...config.RetentionRule) harnessOpt {
	return func(c *harnessCfg) { c.rules = r }
}
func withRewrite(r ...config.RewriteRule) harnessOpt { return func(c *harnessCfg) { c.rewrite = r } }
func withVerify(v config.VerifyConfig) harnessOpt    { return func(c *harnessCfg) { c.verify = &v } }

// withEnforce turns on runtime enforcement (quarantine) for the `edge` engine
// store and a verdict cache. Combine with withVerify to supply the trust store.
func withEnforce() harnessOpt { return func(c *harnessCfg) { c.enforce = true } }
func withStoreVerify(store string, v config.StoreVerify) harnessOpt {
	return func(c *harnessCfg) {
		if c.storeVerify == nil {
			c.storeVerify = map[string]*config.StoreVerify{}
		}
		c.storeVerify[store] = &v
	}
}

// newHarness brings up remote + cache in-memory registries, a fake engine, and a
// real gantry server over bufconn, and returns a connected client. Everything is
// torn down via t.Cleanup.
func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	var hc harnessCfg
	hc.cacheMode = "copy"
	for _, o := range opts {
		o(&hc)
	}

	dir := t.TempDir()
	remoteHost, closeRemote := newRegistry(t)

	var cacheHost string
	var uploads *int32
	var closeCache func()
	cacheStore := config.StoreConfig{Kind: "oci", Mode: hc.cacheMode, Rewrite: hc.rewrite}
	if hc.tlsCache {
		var caFile string
		cacheHost, caFile, closeCache = newTLSRegistry(t)
		cacheStore.Host = cacheHost
		cacheStore.CACert = caFile
		uploads = new(int32) // upload counting is not wired for the TLS variant
	} else {
		cacheHost, uploads, closeCache = newCountingRegistry(t)
		cacheStore.Host = cacheHost
		cacheStore.Insecure = true
	}
	remoteStore := config.StoreConfig{Kind: "oci", Host: remoteHost, Insecure: true}
	if hc.storeVerify != nil {
		if v := hc.storeVerify["remote"]; v != nil {
			remoteStore.Verify = v
		}
		if v := hc.storeVerify["cache"]; v != nil {
			cacheStore.Verify = v
		}
	}
	rules := hc.rules
	if rules == nil {
		rules = []config.RetentionRule{{Repo: "**"}}
	}
	edgeStore := config.StoreConfig{
		Kind:    "docker",
		Address: "unix:///nowhere",
		Retention: &config.StoreRetention{
			Path:  filepath.Join(dir, "edge.db"),
			Rules: rules,
		},
	}

	cfg := &config.Config{
		Serve: config.ServeConfig{
			Addr:   "127.0.0.1:0",
			Events: config.EventsConfig{Path: filepath.Join(dir, "events.db"), Cap: 1000},
		},
		Stores: map[string]config.StoreConfig{
			"remote": remoteStore,
			"cache":  cacheStore,
			"edge":   edgeStore,
		},
	}
	if hc.verify != nil {
		cfg.Serve.Verify = *hc.verify
	}
	if hc.enforce {
		cfg.Serve.Verify.Cache = config.VerifyCacheConfig{Path: filepath.Join(dir, "verify.db")}
		cfg.Serve.Enforce = config.EnforceConfig{
			Mode: "quarantine", Stores: []string{"edge"}, OnUnavailable: "grace",
		}
	}
	if err := cfg.Evaluate(); err != nil {
		t.Fatalf("evaluate config: %v", err)
	}

	// Build the store set with only the registries dialed; inject the fake engine.
	set, err := store.NewSet(map[string]config.StoreConfig{
		"remote": cfg.Stores["remote"],
		"cache":  cfg.Stores["cache"],
	}, false)
	if err != nil {
		t.Fatalf("store set: %v", err)
	}
	eng := newFakeEngine("edge")
	edgeCfg := cfg.Stores["edge"]
	edgeCfg.Name = "edge"
	set.PutEngine(edgeCfg, eng)

	clk := newClock()
	ctx, cancel := context.WithCancel(context.Background())

	srv, err := app.Build(ctx, cfg, app.WithStoreSet(set), app.WithNow(clk.Now))
	if err != nil {
		cancel()
		t.Fatalf("build server: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.GRPC.Serve(lis) }()

	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
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
		closeCache()
		closeRemote()
	})

	return &harness{
		t:            t,
		client:       pb.NewClient(conn),
		conn:         conn,
		engine:       eng,
		clock:        clk,
		remote:       remoteHost,
		cache:        cacheHost,
		cacheUploads: uploads,
	}
}

// --- job helpers ---

// add submits a job and returns its id. trailers, if non-nil, receives the
// response trailer metadata.
func (h *harness) add(req *pb.JobAddRequest, callOpts ...grpc.CallOption) *pb.Job {
	h.t.Helper()
	job, err := h.client.Job().Add(context.Background(), req, callOpts...)
	if err != nil {
		h.t.Fatalf("job add: %v", err)
	}
	return job
}

func (h *harness) get(id string) *pb.Job {
	h.t.Helper()
	job, err := h.client.Job().Get(context.Background(), pb.JobGetById(id))
	if err != nil {
		h.t.Fatalf("job get %s: %v", id, err)
	}
	return job
}

// imageList returns the retention inventory records for a store.
func (h *harness) imageList(store string) []*pb.Image {
	h.t.Helper()
	res, err := h.client.Image().List(context.Background(), pb.ImageListRequest_builder{
		Store: pb.StoreByName(store),
	}.Build())
	if err != nil {
		h.t.Fatalf("image list %s: %v", store, err)
	}
	return res.GetItems()
}

// gcApply runs a GC pass on a store with the configured rules (no override).
func (h *harness) gcApply(store string) *pb.StoreGcApplyResponse {
	h.t.Helper()
	res, err := h.client.Store().GcApply(context.Background(), pb.StoreGcRequest_builder{
		Store: pb.StoreByName(store),
	}.Build())
	if err != nil {
		h.t.Fatalf("gc apply %s: %v", store, err)
	}
	return res
}

// waitDone polls until the job reaches a terminal state or the deadline, then
// returns the final snapshot.
func (h *harness) waitDone(id string) *pb.Job {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		job := h.get(id)
		switch job.GetState() {
		case pb.JobState_JOB_STATE_DONE, pb.JobState_JOB_STATE_FAILED, pb.JobState_JOB_STATE_CANCELED:
			return job
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("job %s did not terminate; last state %v", id, job.GetState())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
