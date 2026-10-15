// Package app composes every Fx module into the application.
//
// Modules are listed in dependency order. Fx starts them in the order their
// constructors run and stops them in reverse, so the HTTP server and workers
// stop before the PostgreSQL pool and the SQS client they use.
package app

import (
	"log/slog"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/httpapi"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/outbox"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/pendingref"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/retention"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqsclient"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqsconsumer"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqspublisher"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/logger"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/metrics"
)

// New returns the application options for an already validated config.
func New(cfg config.Config) fx.Option {
	return fx.Options(
		fx.Supply(cfg),
		fx.StartTimeout(cfg.ShutdownTimeout),
		fx.StopTimeout(cfg.ShutdownTimeout),
		fx.WithLogger(func(log *slog.Logger) fxevent.Logger {
			l := &fxevent.SlogLogger{Logger: log}
			l.UseLogLevel(slog.LevelDebug)
			return l
		}),

		logger.Module,
		metrics.Module,
		health.Module,
		postgres.Module,
		sqsclient.Module,
		auth.Module,
		usecases,
		sqspublisher.Module,
		outbox.Module,
		sqsconsumer.Module,
		pendingref.Module,
		retention.Module,
		httpapi.Module,
	)
}
