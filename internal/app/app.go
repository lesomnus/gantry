// Package app wires a complete, running gantry server from configuration. The
// same builder backs the `serve` command and the E2E test harness, so tests
// exercise production wiring rather than a parallel copy that could drift.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/cpx"
	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/rpc"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/z"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
)

// Server is a fully wired gantry server whose background workers (the copier,
// the retention watchers and scheduler, and the readiness loop) are already
// started on the context passed to Build. The caller owns the listener and the
// shutdown sequence: Serve on GRPC, then GracefulStop it, then call Stop.
type Server struct {
	GRPC   *grpc.Server
	RPC    *rpc.Server
	copier *cpx.Copier

	closers []func() error
}

// Stop drains the copier and releases resources (retention indices, the audit
// log, the store set). Call it after the gRPC server has stopped serving.
func (s *Server) Stop() {
	s.copier.Stop()
	for i := len(s.closers) - 1; i >= 0; i-- {
		_ = s.closers[i]()
	}
}

type options struct {
	now    func() time.Time
	stores *store.Set
}

// Option configures Build.
type Option func(*options)

// WithNow injects a clock into the retention manager and the audit log, so
// time-dependent behavior (GC grace/age, event timestamps) is deterministic in
// tests. Production leaves it unset (time.Now).
func WithNow(now func() time.Time) Option { return func(o *options) { o.now = now } }

// WithStoreSet supplies a pre-built store set instead of dialing one from
// config, so tests can inject fake engine daemons via Set.PutEngine. Build still
// reads c.Stores for the per-store retention configuration and owns closing the
// set. Production leaves it unset (Build calls store.NewSet).
func WithStoreSet(s *store.Set) Option { return func(o *options) { o.stores = s } }

// Build assembles the whole server from c and starts its background workers on
// ctx. It does not listen or serve — the caller supplies the listener (a real
// net.Listener in `serve`, a bufconn in tests). On any failure every resource
// opened so far is released before returning.
func Build(ctx context.Context, c *config.Config, opts ...Option) (_ *Server, err error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	nowFn := time.Now
	var evOpts []event.Option
	var gcOpts []retention.Option
	if o.now != nil {
		nowFn = o.now
		evOpts = append(evOpts, event.WithNow(o.now))
		gcOpts = append(gcOpts, retention.WithNow(o.now))
	}

	var closers []func() error
	defer func() {
		if err != nil {
			for i := len(closers) - 1; i >= 0; i-- {
				_ = closers[i]()
			}
		}
	}()

	stores := o.stores
	if stores == nil {
		stores, err = store.NewSet(c.Stores, c.Serve.AllowUnknownStores)
		if err != nil {
			return nil, z.Err(err, "build stores")
		}
	}
	closers = append(closers, stores.Close)

	// Retention is per-store: each engine store with a retention block gets its
	// own index, rules, and scheduler. There is no global policy.
	var gc *retention.Manager
	{
		var gcStores []retention.Store
		for name, sc := range c.Stores {
			if !sc.Retention.Enabled() {
				continue
			}
			eng, err := stores.Engine(name)
			if err != nil {
				return nil, z.Err(err, "retention store %q", name)
			}
			rc := sc.Retention
			ix, err := retention.Open(rc.Path)
			if err != nil {
				return nil, z.Err(err, "open retention index for %q", name)
			}
			closers = append(closers, ix.Close)
			rules := make([]retention.Rule, len(rc.Rules))
			for i, rr := range rc.Rules {
				rules[i] = retention.Rule{
					Repo:    rr.Repo,
					MaxAge:  (*time.Duration)(rr.MaxAge),
					KeepN:   rr.KeepN,
					MaxN:    rr.MaxN,
					MaxIdle: (*time.Duration)(rr.MaxIdle),
					Pins:    rr.Pins,
				}
			}
			gcStores = append(gcStores, retention.Store{
				Name:   name,
				Engine: eng,
				Index:  ix,
				Rules:  rules,
				Schedule: retention.Schedule{
					Interval:    time.Duration(rc.Interval),
					MinInterval: time.Duration(rc.MinInterval),
					Grace:       time.Duration(rc.Grace),
					Heartbeat:   rc.HeartbeatInterval(),
				},
				UntaggedAfter: rc.UntaggedReapAfter(),
			})
			log.From(ctx).Info("retention enabled",
				slog.String("store", name), slog.String("path", rc.Path), slog.Int("rules", len(rc.Rules)))
		}
		if len(gcStores) > 0 {
			gc = retention.NewManager(gcStores, gcOpts...)
		}
	}

	hc := health.NewChecker(stores, health.Options{
		CacheTTL:     time.Duration(c.Serve.Health.CacheTTL),
		ProbeTimeout: time.Duration(c.Serve.Health.ProbeTimeout),
		ReadyStores:  c.Serve.Health.ReadyStores,
	})

	var events *event.Log
	if c.Serve.Events.Enabled() {
		ev, err := event.Open(c.Serve.Events.Path, c.Serve.Events.Cap, evOpts...)
		if err != nil {
			return nil, z.Err(err, "open audit log")
		}
		closers = append(closers, ev.Close)
		events = ev
		if gc != nil {
			gc.SetRecorder(event.NewRecorder(ev, log.From(ctx)))
		}
		log.From(ctx).Info("audit log enabled", slog.String("path", c.Serve.Events.Path))
	}

	jobStore := cpx.NewMemStore()
	wmr := cpx.NewCopier(stores, jobStore, c.Worker)
	if events != nil {
		wmr.SetRecorder(event.NewRecorder(events, log.From(ctx)))
	}
	if gc != nil {
		// Stamp the retention index when a job's engine destination pulls, so the
		// image is age-collectable like any other tracked pull.
		wmr.SetPullHook(func(engine, ref string) { gc.Distributed(engine, ref, nowFn()) })
	}
	// The interface type matters: a nil *Swappable in a verify.Service interface
	// is non-nil and would bypass every disabled-guard.
	var vf verify.Service
	if c.VerifyEnabled() {
		v, err := verify.NewSwappable(c.Serve.Verify)
		if err != nil {
			return nil, z.Err(err, "signature verification setup") // fail fast: don't serve unsafe
		}
		vf = v
		wmr.SetVerifier(v)
		l := log.From(ctx)
		l.Info("signature verification enabled",
			slog.String("mode", string(c.Serve.Verify.Mode)),
			slog.String("provider", c.Serve.Verify.Provider))
		if c.Serve.Verify.Level != "" && c.Serve.Verify.Level != "strict" {
			l.Warn("signature verification level is not strict: certificate expiry and revocation are not enforced",
				slog.String("level", c.Serve.Verify.Level))
		}
	}

	wmr.Start(ctx)
	if gc != nil {
		gc.StartWatchers(ctx)
		gc.StartScheduler(ctx)
	}

	au, as := rpc.Auth(c.Serve.Auth)
	sopts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(au),
		grpc.ChainStreamInterceptor(as),
	}
	if c.Serve.Auth.TLSCert != "" {
		creds, err := credentials.NewServerTLSFromFile(c.Serve.Auth.TLSCert, c.Serve.Auth.TLSKey)
		if err != nil {
			return nil, z.Err(err, "tls")
		}
		sopts = append(sopts, grpc.Creds(creds))
	}
	gsrv := grpc.NewServer(sopts...)
	rsrv := rpc.New(wmr, jobStore, stores, gc, hc, vf, events)
	hs := rsrv.Register(gsrv)
	reflection.Register(gsrv)

	// Readiness rides the standard health service: the overall status follows the
	// gated stores.
	go rsrv.WatchReadiness(ctx, hs, 5*time.Second)

	return &Server{GRPC: gsrv, RPC: rsrv, copier: wmr, closers: closers}, nil
}
