package sqsconsumer

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqsclient"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// Module provides the consumer and starts it with the application. It is registered
// after the use case and before the HTTP server, and Fx stops in the reverse order of
// starting: the HTTP server stops first, then the consumer stops receiving and finishes
// (or releases) the messages in flight, and only then the database pool and the SQS
// client are closed.
var Module = fx.Module("sqsconsumer",
	fx.Provide(NewFromConfig),
	fx.Invoke(func(*Consumer) {}),
)

// NewFromConfig builds the consumer from the configuration and ties it to the Fx
// lifecycle. The consumer is named after the input queue in the inbox.
func NewFromConfig(lc fx.Lifecycle, cfg config.Config, client sqsclient.Consumer, uc *processwager.UseCase, log *slog.Logger, reg *prometheus.Registry) *Consumer {
	c := New(client, uc, Options{
		Name:              cfg.SQS.InputQueue,
		InputQueue:        cfg.SQS.InputQueue,
		DeadLetterQueue:   cfg.SQS.DeadLetterQueue,
		Workers:           cfg.SQS.Consumer.Workers,
		BatchSize:         cfg.SQS.Consumer.BatchSize,
		PollWait:          cfg.SQS.Consumer.PollWait,
		ProcessingTimeout: cfg.SQS.Consumer.ProcessingTimeout,
		BackoffBase:       cfg.SQS.Consumer.RetryBackoffBase,
		BackoffMax:        cfg.SQS.Consumer.RetryBackoffMax,
		Logger:            log,
		Registerer:        reg,
	})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			c.Start()
			return nil
		},
		OnStop: c.Stop,
	})
	return c
}
