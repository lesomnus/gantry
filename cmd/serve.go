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

	"github.com/lesomnus/gantry/internal/server"
	"github.com/lesomnus/gantry/internal/store"
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

			stores, err := store.NewSet(c.Serve.Stores, c.Serve.AllowUnknownStores)
			if err != nil {
				return z.Err(err, "build stores")
			}
			defer stores.Close()

			jobStore := warm.NewMemStore()
			wmr := warm.NewWarmer(stores, jobStore, c.Serve.Warm)

			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			wmr.Start(ctx)

			h := server.Auth(c.Serve.Auth)(server.New(wmr, jobStore, stores))
			srv := &http.Server{
				Addr:        c.Serve.Addr,
				Handler:     h,
				BaseContext: func(net.Listener) context.Context { return ctx },
			}

			l := log.From(ctx)
			l.Info("serving", slog.String("addr", c.Serve.Addr), slog.Int("stores", len(c.Serve.Stores)))

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
