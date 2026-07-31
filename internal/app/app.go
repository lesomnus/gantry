// Package app wires a complete, running gantry server from configuration. The
// same builder backs the `serve` command and the E2E test harness, so tests
// exercise production wiring rather than a parallel copy that could drift.
package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/lesomnus/gantry/cmd/config"
	"github.com/lesomnus/gantry/internal/cpx"
	"github.com/lesomnus/gantry/internal/enforce"
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
			rules := make([]retention.Rule, 0, len(rc.Rules))
			for _, rr := range rc.Rules {
				rule := retention.Rule{
					Repo:    rr.Repo,
					MaxAge:  (*time.Duration)(rr.MaxAge),
					KeepN:   rr.KeepN,
					MaxN:    rr.MaxN,
					MaxIdle: (*time.Duration)(rr.MaxIdle),
					Pins:    rr.Pins,
				}
				rules = append(rules, rule)
				// A routed job lands the image under the CACHE's host, so a rule
				// written for the origin would not match what the node holds and the
				// image would sit unmanaged forever. Cover the caches the origin
				// declares, so the operator states the rule once.
				for _, alias := range c.RouteAliases(name, rr.Repo) {
					aliased := rule
					aliased.Repo = alias
					rules = append(rules, aliased)
					log.From(ctx).Debug("retention rule covers a routed cache",
						slog.String("store", name), slog.String("rule", rr.Repo), slog.String("also", alias))
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
	var vf verify.Service    // the verifier the copy path + RPC see (cache-wrapped)
	var vraw verify.Verifier // the raw verifier (bypasses the cache; used by the refresher)
	var vcache *verify.Cache // the shared verdict cache, nil when unconfigured
	if c.NeedVerifier() {    // verification anywhere, OR enforcement is on
		v, err := verify.NewSwappable(c.Serve.Verify)
		if err != nil {
			return nil, z.Err(err, "signature verification setup") // fail fast: don't serve unsafe
		}
		vf, vraw = v, v
		l := log.From(ctx)
		l.Info("signature verification enabled",
			slog.String("mode", string(c.Serve.Verify.Mode)),
			slog.String("provider", c.Serve.Verify.Provider))
		if c.Serve.Verify.Level != "" && c.Serve.Verify.Level != "strict" {
			l.Warn("signature verification level is not strict: certificate expiry and revocation are not enforced",
				slog.String("level", c.Serve.Verify.Level))
		}
		if c.Serve.Verify.LocalLayoutEnabled() {
			l.Info("offline local signature layout enabled", slog.String("local_layout", c.Serve.Verify.LocalLayout))
		}
		// Durable verdict cache: wrap the verifier so admission populates it and
		// enforcement/RPC read it. The copy path and RPC see the cache-wrapped vf.
		if vc := c.Serve.Verify.Cache; vc.Enabled() {
			cache, err := verify.OpenCache(vc.Path, time.Duration(vc.TTL), time.Duration(vc.Refresh), verify.WithNow(nowFn))
			if err != nil {
				return nil, z.Err(err, "open verify cache")
			}
			closers = append(closers, cache.Close)
			vcache = cache
			vf = verify.NewCaching(v, cache, c.Serve.Verify, verify.CachingWithNow(nowFn))
			l.Info("verification result cache enabled", slog.String("path", vc.Path),
				slog.Duration("ttl", time.Duration(vc.TTL)), slog.Duration("refresh", time.Duration(vc.Refresh)))
		}
		wmr.SetVerifier(vf)
	}

	// Runtime signature enforcement: watch engine start events and quarantine
	// containers whose image is not signed by a trusted Root CA.
	var enf *enforce.Manager
	if c.Serve.Enforce.Enabled() {
		var enfStores []enforce.Store
		for _, name := range c.Serve.Enforce.Stores {
			eng, err := stores.Engine(name)
			if err != nil {
				return nil, z.Err(err, "enforce store %q", name)
			}
			ee, ok := eng.(enforce.Engine)
			if !ok {
				return nil, z.Err(nil, "store %q does not support runtime enforcement (containerd is not yet supported)", name)
			}
			enfStores = append(enfStores, enforce.Store{Name: name, Engine: ee})
		}
		enf = enforce.NewManager(enfStores, vcache, vf, c.Stores, enforce.Options{
			OnUnavailable: c.Serve.Enforce.OnUnavailable,
			SelfContainer: c.Serve.Enforce.SelfContainer,
			Now:           nowFn,
		})
		closers = append(closers, func() error { enf.Stop(); return nil })
		log.From(ctx).Info("runtime enforcement enabled",
			slog.Any("stores", c.Serve.Enforce.Stores), slog.String("on_unavailable", c.Serve.Enforce.OnUnavailable))
	}

	wmr.Start(ctx)
	if gc != nil {
		gc.StartWatchers(ctx)
		gc.StartScheduler(ctx)
	}
	if enf != nil {
		enf.StartWatchers(ctx)
		// Keep trusted verdicts fresh within the soft-refresh window using the raw
		// verifier (so a re-check reaches the registry / layout, not the cache).
		if vcache != nil {
			resolve := func(sourceRef string) (config.StoreConfig, name.Reference, bool) {
				r, err := name.ParseReference(sourceRef, name.Insecure)
				if err != nil {
					return config.StoreConfig{}, nil, false
				}
				for _, s := range c.Stores {
					if s.IsRegistry() && s.Host == r.Context().RegistryStr() {
						return s, r, true
					}
				}
				return config.StoreConfig{}, nil, false
			}
			go verify.NewRefresher(vcache, vraw, resolve, verify.RefresherWithNow(nowFn)).Run(ctx)
		}
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
