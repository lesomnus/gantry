package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/lesomnus/gantry/internal/app"
	"github.com/lesomnus/gantry/internal/rpc"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"google.golang.org/grpc"
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

			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			srv, err := app.Build(ctx, c)
			if err != nil {
				return err
			}
			defer srv.Stop()

			lis, err := net.Listen("tcp", c.Serve.Addr)
			if err != nil {
				return z.Err(err, "listen")
			}

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
				if err := srv.GRPC.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
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
			go func() { srv.GRPC.GracefulStop(); close(done) }()
			select {
			case <-done:
			case <-sctx.Done():
				l.Warn("graceful shutdown timed out")
				srv.GRPC.Stop()
			}
			return nil
		}),
	}
}
