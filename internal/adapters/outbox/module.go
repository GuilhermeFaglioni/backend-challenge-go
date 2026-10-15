package outbox

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// Module provides the publisher and starts it with the application. Fx stops
// components in the reverse order they started, so the publisher stops after the HTTP
// server (nothing writes new events) and before the database pool and the SQS client
// it uses are closed.
var Module = fx.Module("outbox",
	fx.Provide(NewFromConfig),
	fx.Invoke(func(*Publisher) {}),
)

// NewFromConfig builds the publisher from the configuration and ties it to the Fx
// lifecycle.
func NewFromConfig(lc fx.Lifecycle, cfg config.Config, store application.OutboxStore, sender application.MessageSender, log *slog.Logger, reg *prometheus.Registry) *Publisher {
	p := New(store, sender, Options{
		PollInterval: cfg.Outbox.PollInterval,
		BatchSize:    cfg.Outbox.BatchSize,
		Lease:        cfg.Outbox.Lease,
		Logger:       log,
		Registerer:   reg,
	})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			p.Start()
			return nil
		},
		OnStop: p.Stop,
	})
	return p
}
