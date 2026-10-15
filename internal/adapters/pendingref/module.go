package pendingref

import (
	"context"
	"log/slog"

	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/promrec"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/resolvepending"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// Module provides the worker and starts it with the application. Fx stops in the reverse
// order of starting: the HTTP server and the queue consumer stop first (nothing new is
// accepted), then this worker finishes the try in hand, and only then are the database
// pool and the clients closed.
var Module = fx.Module("pendingref",
	fx.Provide(NewFromConfig),
	fx.Invoke(func(*Worker) {}),
)

// NewFromConfig builds the worker from the configuration and ties it to the Fx lifecycle.
func NewFromConfig(lc fx.Lifecycle, cfg config.Config, store application.PendingStore, uc *resolvepending.UseCase, metrics *promrec.Pending, log *slog.Logger) *Worker {
	p := cfg.PendingReference
	w := New(store, uc, Options{
		PollInterval:     p.PollInterval,
		BatchSize:        p.BatchSize,
		Lease:            p.Lease,
		Logger:           log,
		Observer:         metrics,
		Recorder:         metrics,
		MaxErrors:        p.MaxErrors,
		ErrorBackoffBase: p.BackoffBase,
		ErrorBackoffMax:  p.BackoffMax,
	})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			w.Start()
			return nil
		},
		OnStop: w.Stop,
	})
	return w
}
