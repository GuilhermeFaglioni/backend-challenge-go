// Package postgres holds the PostgreSQL connection pool, the UnitOfWork and the
// repositories bound to its transaction (see ARCHITECTURE.md §6).
package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

var Module = fx.Module("postgres",
	fx.Provide(
		NewPool,
		fx.Annotate(newUnitOfWorkFromFx, fx.As(new(application.UnitOfWork))),
		fx.Annotate(NewWalletReader, fx.As(new(application.WalletReader))),
		fx.Annotate(NewOutboxStore, fx.As(new(application.OutboxStore))),
		fx.Annotate(NewPendingStore, fx.As(new(application.PendingStore))),
		fx.Annotate(NewRetentionStore, fx.As(new(application.RetentionStore))),
		fx.Annotate(NewTransactionReader, fx.As(new(application.TransactionReader))),
		fx.Annotate(NewLedgerReader, fx.As(new(application.LedgerReader))),
		fx.Annotate(NewLedgerAuditor, fx.As(new(application.LedgerAuditor))),
		fx.Annotate(newReadinessCheck, fx.ResultTags(`group:"readiness"`)),
	),
)

// NewPool creates the pool and ties it to the Fx lifecycle: the database must
// answer a ping before the application starts, and the pool is closed only
// after every component that depends on it has stopped.
func NewPool(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.Database.MaxConns

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return fmt.Errorf("ping postgres: %w", err)
			}
			log.InfoContext(ctx, "postgres pool ready", slog.Int("maxConns", int(poolCfg.MaxConns)))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			pool.Close()
			log.InfoContext(ctx, "postgres pool closed")
			return nil
		},
	})
	return pool, nil
}

func newReadinessCheck(pool *pgxpool.Pool) health.Check {
	return health.Check{Name: "postgres", Fn: pool.Ping}
}

type unitOfWorkParams struct {
	fx.In

	Pool *pgxpool.Pool
	// Observer is told about every retry; the application provides one for metrics.
	Observer application.RetryObserver `optional:"true"`
}

func newUnitOfWorkFromFx(p unitOfWorkParams) *UnitOfWork {
	u := NewUnitOfWork(p.Pool)
	if p.Observer != nil {
		u = u.WithObserver(p.Observer)
	}
	return u
}
