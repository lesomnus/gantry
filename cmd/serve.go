package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/lesomnus/gantry/internal/cpx"
	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/rpc"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
)

func NewCmdServe() *xli.Command {
	return &xli.Command{
		Name:  "serve",
		Brief: "run the image copy API server",

		Flags: flg.Flags{
			&flg.String{Name: "addr", Brief: "listen address (overrides config)"},
		},

		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			flg.VisitP(cmd, "addr", &c.Serve.Addr)

			stores, err := store.NewSet(c.Stores, c.Serve.AllowUnknownStores)
			if err != nil {
				return z.Err(err, "build stores")
			}
			defer stores.Close()

			// Retention is per-store: each engine store with a retention block gets
			// its own index, rules, and scheduler. There is no global policy.
			var gc *retention.Manager
			{
				var gcStores []retention.Store
				for name, sc := range c.Stores {
					if !sc.Retention.Enabled() {
						continue
					}
					eng, err := stores.Engine(name)
					if err != nil {
						return z.Err(err, "retention store %q", name)
					}
					rc := sc.Retention
					ix, err := retention.Open(rc.Path)
					if err != nil {
						return z.Err(err, "open retention index for %q", name)
					}
					rules := make([]retention.Rule, len(rc.Rules))
					for i, rr := range rc.Rules {
						rules[i] = retention.Rule{
							Repo:   rr.Repo,
							MaxAge: (*time.Duration)(rr.MaxAge),
							KeepN:  rr.KeepN,
							MaxN:   rr.MaxN,
							Pins:   rr.Pins,
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
						},
						UntaggedAfter: rc.UntaggedReapAfter(),
					})
					log.From(ctx).Info("retention enabled",
						slog.String("store", name), slog.String("path", rc.Path), slog.Int("rules", len(rc.Rules)))
				}
				if len(gcStores) > 0 {
					gc = retention.NewManager(gcStores)
					defer gc.Close()
				}
			}

			hc := health.NewChecker(stores, health.Options{
				CacheTTL:     time.Duration(c.Serve.Health.CacheTTL),
				ProbeTimeout: time.Duration(c.Serve.Health.ProbeTimeout),
				ReadyStores:  c.Serve.Health.ReadyStores,
			})

			var events *event.Log
			if c.Serve.Events.Enabled() {
				ev, err := event.Open(c.Serve.Events.Path, c.Serve.Events.Cap)
				if err != nil {
					return z.Err(err, "open audit log")
				}
				defer ev.Close()
				events = ev
				rec := event.NewRecorder(ev)
				if gc != nil {
					gc.SetRecorder(rec)
				}
				log.From(ctx).Info("audit log enabled", slog.String("path", c.Serve.Events.Path))
			}

			jobStore := cpx.NewMemStore()
			wmr := cpx.NewCopier(stores, jobStore, c.Worker)
			if events != nil {
				wmr.SetRecorder(event.NewRecorder(events))
			}
			if gc != nil {
				// Stamp the retention index when a job's engine destination pulls, so
				// the image is age-collectable like any other tracked pull.
				wmr.SetPullHook(func(engine, ref string) { gc.Distributed(engine, ref, time.Now()) })
			}
			// The interface type matters: a nil *Swappable in a verify.Service
			// interface is non-nil and would bypass every disabled-guard.
			var vf verify.Service
			if c.VerifyEnabled() {
				v, err := verify.NewSwappable(c.Serve.Verify)
				if err != nil {
					return z.Err(err, "signature verification setup") // fail fast: don't serve unsafe
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

			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			wmr.Start(ctx)
			if gc != nil {
				gc.StartWatchers(ctx)
				gc.StartScheduler(ctx)
			}

			au, as := rpc.Auth(c.Serve.Auth)
			opts := []grpc.ServerOption{
				grpc.ChainUnaryInterceptor(au),
				grpc.ChainStreamInterceptor(as),
			}
			if c.Serve.Auth.TLSCert != "" {
				creds, err := credentials.NewServerTLSFromFile(c.Serve.Auth.TLSCert, c.Serve.Auth.TLSKey)
				if err != nil {
					return z.Err(err, "tls")
				}
				opts = append(opts, grpc.Creds(creds))
			}
			gsrv := grpc.NewServer(opts...)
			rsrv := rpc.New(wmr, jobStore, stores, gc, hc, vf, events)
			hs := rsrv.Register(gsrv)
			reflection.Register(gsrv)

			lis, err := net.Listen("tcp", c.Serve.Addr)
			if err != nil {
				return z.Err(err, "listen")
			}
			// Readiness rides the standard health service: the overall status
			// follows the gated stores, like the old /readyz.
			go rsrv.WatchReadiness(ctx, hs, 5*time.Second)

			l := log.From(ctx)
			l.Info("serving", slog.String("addr", c.Serve.Addr), slog.Int("stores", len(c.Stores)))
			if !rpc.AuthEnabled(c.Serve.Auth) {
				l.Warn("API authentication is DISABLED — every RPC is unauthenticated, "+
					"including destructive ones (StoreService.Remove/GcApply, PinService). "+
					"Set serve.auth.tokens or place gantry behind an authenticating proxy.",
					slog.String("addr", c.Serve.Addr))
			}

			errc := make(chan error, 1)
			go func() {
				if err := gsrv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
					errc <- err
				}
			}()

			select {
			case err := <-errc:
				return z.Err(err, "serve")
			case <-ctx.Done():
				l.Info("shutting down")
			}

			// Drain in-flight RPCs within the grace window.
			grace := time.Duration(c.Serve.ShutdownGrace)
			sctx, cancel := context.WithTimeout(context.Background(), grace)
			defer cancel()
			done := make(chan struct{})
			go func() { gsrv.GracefulStop(); close(done) }()
			select {
			case <-done:
			case <-sctx.Done():
				l.Warn("graceful shutdown timed out")
				gsrv.Stop()
			}
			wmr.Stop()
			return nil
		}),
	}
}
