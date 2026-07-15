package rpc_test

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/down"
	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/rpc"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthsvc "google.golang.org/grpc/health"
	"google.golang.org/grpc/test/bufconn"
)

// fakeEngine is a canned down.Engine. readyErr is mutex-guarded because the
// readiness watcher probes it concurrently with the test's writes.
type fakeEngine struct {
	name      string
	inuse     map[string]bool
	removeRes down.RemoveResult
	pullErr   error
	removeErr error
	inuseErr  error

	mu       sync.Mutex
	readyErr error

	pulled  []pullCall
	removed []string
}

type pullCall struct {
	ref, digest, platform string
	as                    []string
}

func (e *fakeEngine) setReady(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readyErr = err
}

func (e *fakeEngine) Name() string { return e.name }
func (e *fakeEngine) Kind() string { return "docker" }

func (e *fakeEngine) Ready(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readyErr
}
func (e *fakeEngine) Platform(context.Context) (string, error) { return "linux/amd64", nil }
func (e *fakeEngine) Close() error                             { return nil }

func (e *fakeEngine) Pull(_ context.Context, ref, digest, platform string, as []string, _ down.Sink) error {
	if e.pullErr != nil {
		return e.pullErr
	}
	e.pulled = append(e.pulled, pullCall{ref, digest, platform, as})
	return nil
}

func (e *fakeEngine) InUse(context.Context) (map[string]bool, error) {
	if e.inuseErr != nil {
		return nil, e.inuseErr
	}
	out := make(map[string]bool, len(e.inuse))
	for k, v := range e.inuse {
		out[k] = v
	}
	return out, nil
}

func (e *fakeEngine) SeedUsage(context.Context, down.UsageSink) error { return nil }
func (e *fakeEngine) WatchUsage(ctx context.Context, _ down.UsageSink) error {
	<-ctx.Done()
	return ctx.Err()
}

func (e *fakeEngine) Remove(_ context.Context, ref string) (down.RemoveResult, error) {
	if e.removeErr != nil {
		return down.RemoveResult{}, e.removeErr
	}
	e.removed = append(e.removed, ref)
	return e.removeRes, nil
}

// fakeWarmer is a canned rpc.Warmer.
type fakeWarmer struct {
	snap    warm.JobSnapshot
	created bool
	err     error
	planRes warm.PlanResult
	planErr error

	submits []warm.Request
	retries []string
}

func (w *fakeWarmer) Submit(req warm.Request) (warm.JobSnapshot, bool, error) {
	w.submits = append(w.submits, req)
	return w.snap, w.created, w.err
}

func (w *fakeWarmer) Retry(id string) (warm.JobSnapshot, bool, error) {
	w.retries = append(w.retries, id)
	return w.snap, w.created, w.err
}

func (w *fakeWarmer) Plan(context.Context, warm.Request) (warm.PlanResult, error) {
	return w.planRes, w.planErr
}

// fakeVerify is a canned verify.Service.
type fakeVerify struct {
	desc      verify.Description
	res       verify.Result
	err       error
	reloadErr error
}

func (v *fakeVerify) Verify(context.Context, config.StoreConfig, name.Reference) (verify.Result, error) {
	return v.res, v.err
}

func (v *fakeVerify) Describe() verify.Description { return v.desc }
func (v *fakeVerify) Reload() (verify.Description, error) {
	return v.desc, v.reloadErr
}

type env struct {
	eng    *fakeEngine
	warmer *fakeWarmer
	verify *fakeVerify
	jobs   warm.Store
	ix     *retention.Index
	gc     *retention.Manager
	events *event.Log
	stores *store.Set
	srv    *rpc.Server
	hs     *healthsvc.Server

	client pb.Client
	conn   *grpc.ClientConn
}

type envOpt func(*envCfg)

type envCfg struct {
	gcOff     bool
	eventsOff bool
	verifyOn  bool
	typedNils bool
	auth      *config.AuthConfig
}

func withoutGC() envOpt     { return func(c *envCfg) { c.gcOff = true } }
func withoutEvents() envOpt { return func(c *envCfg) { c.eventsOff = true } }
func withVerify() envOpt    { return func(c *envCfg) { c.verifyOn = true } }
func withTypedNils() envOpt { return func(c *envCfg) { c.gcOff = true; c.typedNils = true } }
func withAuth(a config.AuthConfig) envOpt {
	return func(c *envCfg) { c.auth = &a }
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	var cfg envCfg
	for _, o := range opts {
		o(&cfg)
	}

	stores, err := store.NewSet(map[string]config.StoreConfig{
		"src": {Kind: "oci", Host: "src.local", Mode: "copy"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{name: "node", inuse: map[string]bool{}}
	stores.PutEngine(config.StoreConfig{Name: "node", Kind: "docker", Address: "unix:///nowhere"}, eng)

	e := &env{
		eng:    eng,
		warmer: &fakeWarmer{},
		jobs:   warm.NewMemStore(),
		stores: stores,
	}

	var gc rpc.GC
	if !cfg.gcOff {
		ix, err := retention.Open(filepath.Join(t.TempDir(), "ix.db"))
		if err != nil {
			t.Fatal(err)
		}
		mgr := retention.NewManager([]retention.Store{{Name: "node", Engine: eng, Index: ix}})
		t.Cleanup(func() { mgr.Close() })
		e.ix = ix
		e.gc = mgr
		gc = mgr
	}

	if !cfg.eventsOff {
		ev, err := event.Open(filepath.Join(t.TempDir(), "ev.db"), 100)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ev.Close() })
		e.events = ev
	}

	var vf verify.Service
	if cfg.verifyOn {
		e.verify = &fakeVerify{}
		vf = e.verify
	}
	if cfg.typedNils {
		// Production hands rpc.New possibly-nil concrete pointers inside the
		// interfaces; the disabled guards must still fire.
		gc = (*retention.Manager)(nil)
		vf = (*verify.Swappable)(nil)
	}

	hc := health.NewChecker(stores, health.Options{
		CacheTTL:     time.Millisecond,
		ProbeTimeout: time.Second,
	})
	srv := rpc.New(e.warmer, e.jobs, stores, gc, hc, vf, e.events)
	e.srv = srv

	var sopts []grpc.ServerOption
	if cfg.auth != nil {
		au, as := rpc.Auth(*cfg.auth)
		sopts = append(sopts, grpc.ChainUnaryInterceptor(au), grpc.ChainStreamInterceptor(as))
	}
	g := grpc.NewServer(sopts...)
	e.hs = srv.Register(g)

	l := bufconn.Listen(1 << 20)
	go g.Serve(l)
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return l.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	e.conn = conn
	e.client = pb.NewClient(conn)
	return e
}

// addJob seeds a job record in the real in-memory job store.
func (e *env) addJob(t *testing.T, id, ref string, state warm.JobState, at time.Time) *warm.Job {
	t.Helper()
	return e.addLabeledJob(t, id, ref, state, at, nil)
}

// addLabeledJob seeds a job carrying labels; the labels are fixed at Add time
// (they seed the primary handle), so they must be set before the store sees it.
func (e *env) addLabeledJob(t *testing.T, id, ref string, state warm.JobState, at time.Time, labels map[string]string) *warm.Job {
	t.Helper()
	j := warm.NewJob(id, ref, nil, at)
	j.Labels = labels
	_, cancel := context.WithCancel(context.Background())
	j.SetCancel(cancel)
	if err := e.jobs.Add(j); err != nil {
		t.Fatal(err)
	}
	if state != warm.JobPending {
		e.jobs.Update(id, func(j *warm.Job) { j.State = state })
	}
	return j
}
