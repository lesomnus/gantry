package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/server"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

func NewCmdServe() *xli.Command {
	return &xli.Command{
		Name:  "serve",
		Brief: "run the cache-warming API server",

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

			var gc *retention.Manager
			if c.Serve.Retention.Enabled() {
				ix, err := retention.Open(c.Serve.Retention.Path)
				if err != nil {
					return z.Err(err, "open retention index")
				}
				defer ix.Close()
				rc := c.Serve.Retention
				gc = retention.NewManager(ix, stores.Engines(),
					retention.Policy{MaxAge: time.Duration(rc.MaxAge), KeepN: rc.KeepN, MaxN: rc.MaxN, Pins: rc.Pins},
					retention.Schedule{
						Interval:    time.Duration(rc.Interval),
						MinInterval: time.Duration(rc.MinInterval),
						Grace:       time.Duration(rc.Grace),
					})
				log.From(ctx).Info("retention enabled",
					slog.String("path", rc.Path), slog.String("max_age", time.Duration(rc.MaxAge).String()),
					slog.Int("keep_n", rc.KeepN), slog.Int("max_n", rc.MaxN))
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

			jobStore := warm.NewMemStore()
			wmr := warm.NewWarmer(stores, jobStore, c.Worker)
			if events != nil {
				wmr.SetRecorder(event.NewRecorder(events))
			}
			if gc != nil {
				wmr.SetDistributeHook(func(engine, ref string) { gc.Distributed(engine, ref, time.Now()) })
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
				go gc.StartScheduler(ctx)
			}

			h := server.Auth(c.Serve.Auth)(server.New(wmr, jobStore, stores, gc, hc, vf, events))
			srv := &http.Server{
				Addr:        c.Serve.Addr,
				Handler:     h,
				BaseContext: func(net.Listener) context.Context { return ctx },
			}

			l := log.From(ctx)
			l.Info("serving", slog.String("addr", c.Serve.Addr), slog.Int("stores", len(c.Stores)))

			errc := make(chan error, 1)
			go func() {
				var err error
				if c.Serve.Auth.TLSCert != "" {
					err = srv.ListenAndServeTLS(c.Serve.Auth.TLSCert, c.Serve.Auth.TLSKey)
				} else {
					err = srv.ListenAndServe()
				}
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					errc <- err
				}
			}()

			select {
			case err := <-errc:
				return z.Err(err, "listen")
			case <-ctx.Done():
				l.Info("shutting down")
			}

			grace := time.Duration(c.Serve.ShutdownGrace)
			sctx, cancel := context.WithTimeout(context.Background(), grace)
			defer cancel()
			if err := srv.Shutdown(sctx); err != nil {
				l.Warn("graceful shutdown timed out", slog.String("error", err.Error()))
			}
			wmr.Stop()
			return nil
		}),
	}
}
