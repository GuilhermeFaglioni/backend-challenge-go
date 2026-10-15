// Package httpapi is the HTTP adapter: routing, authentication, handlers and the
// server lifecycle. Business endpoints are added as the use cases land, always
// through Routes (routes.go).
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

var Module = fx.Module("httpapi",
	fx.Provide(
		NewServer,
		// The server asks for an Authenticator; the verifier is what satisfies it.
		func(v *auth.Verifier) Authenticator { return v },
		NewWallets,
		func(uc *openwallet.UseCase) WalletOpener { return uc },
		NewWagering,
		NewTransactions,
		func(uc *processwager.UseCase) OperationProcessor { return uc },
	),
	fx.Invoke(func(*http.Server) {}),
)

type ServerParams struct {
	fx.In

	Lifecycle    fx.Lifecycle
	Shutdowner   fx.Shutdowner
	Config       config.Config
	Logger       *slog.Logger
	Health       *health.Registry
	Metrics      *prometheus.Registry
	Authn        Authenticator
	Wallets      *Wallets
	Wagering     *Wagering
	Transactions *Transactions
}

func NewServer(p ServerParams) *http.Server {
	routes := NewRoutes(p.Authn, p.Logger, p.Metrics)
	routes.AllowOrigins(p.Config.CORSAllowedOrigins)
	registerRoutes(routes, p.Health, p.Metrics, p.Wallets, p.Wagering, p.Transactions)

	srv := &http.Server{
		Addr:              p.Config.HTTPAddr,
		Handler:           routes.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(p.Logger.Handler(), slog.LevelError),
	}

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			faultinject.Hit(faultinject.HTTPBeforeListen)
			// Listen synchronously so a busy port fails the startup instead of
			// a background goroutine.
			var lc net.ListenConfig
			ln, err := lc.Listen(ctx, "tcp", srv.Addr)
			if err != nil {
				return err
			}
			faultinject.Hit(faultinject.HTTPAfterListen)
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					p.Logger.Error("http server failed", slog.Any("error", err))
					_ = p.Shutdowner.Shutdown(fx.ExitCode(1))
				}
			}()
			p.Logger.InfoContext(ctx, "http server listening", slog.String("addr", ln.Addr().String()))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Health.MarkDraining()
			err := srv.Shutdown(ctx)
			p.Logger.InfoContext(ctx, "http server stopped")
			return err
		},
	})
	return srv
}
