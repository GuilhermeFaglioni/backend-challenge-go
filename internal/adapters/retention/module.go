package retention

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// Module provides the retention worker and runs it with the application.
var Module = fx.Module("retention",
	fx.Provide(NewFromConfig),
	fx.Invoke(func(*Worker) {}),
)

func NewFromConfig(lc fx.Lifecycle, cfg config.Config, store application.RetentionStore, log *slog.Logger, reg *prometheus.Registry) *Worker {
	w := New(store, Options{
		Inbox: cfg.Retention.Inbox, Outbox: cfg.Retention.Outbox, Interval: cfg.Retention.Interval, BatchSize: cfg.Retention.BatchSize,
		Logger: log, Registerer: reg,
	})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error { w.Start(); return nil },
		OnStop:  w.Stop,
	})
	return w
}
