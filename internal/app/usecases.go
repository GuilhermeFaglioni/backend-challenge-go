package app

import (
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/promrec"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/reconcile"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/resolvepending"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
)

// usecases wires the use cases to their adapters. The Invoke makes Fx build them at
// start-up, so a missing dependency stops the application instead of surfacing on the
// first request, and the use cases' metrics exist at zero from the start.
var usecases = fx.Module("usecases",
	fx.Provide(
		fx.Annotate(
			func(reg *prometheus.Registry) *promrec.OpenWallet { return promrec.NewOpenWallet(reg) },
			fx.As(new(openwallet.Recorder)),
		),
		func(uow application.UnitOfWork, rec openwallet.Recorder) *openwallet.UseCase {
			return openwallet.New(openwallet.Deps{UnitOfWork: uow, Recorder: rec, Now: time.Now, NewID: uuid.New})
		},
	),
	fx.Provide(
		// One recorder serves both roles: it counts the requests and the retries of the
		// units of work they run in.
		func(reg *prometheus.Registry) *promrec.Wager { return promrec.NewWager(reg) },
		func(w *promrec.Wager) processwager.Recorder { return w },
		func(w *promrec.Wager) application.RetryObserver { return w },
		func(cfg config.Config) application.PendingPolicy {
			p := cfg.PendingReference
			return application.PendingPolicy{MaxAttempts: p.MaxAttempts, TTL: p.TTL, BackoffBase: p.BackoffBase, BackoffMax: p.BackoffMax}
		},
		func(uow application.UnitOfWork, rec processwager.Recorder, policy application.PendingPolicy, log *slog.Logger) *processwager.UseCase {
			return processwager.New(processwager.Deps{
				UnitOfWork: uow, Recorder: rec, Logger: log, Now: time.Now, NewID: uuid.New, Pending: policy,
				AfterPending: func() { faultinject.Hit(faultinject.PendingAfterCommit) },
			})
		},
	),
	fx.Provide(
		func(reg *prometheus.Registry) *promrec.Pending { return promrec.NewPending(reg) },
		func(p *promrec.Pending) resolvepending.Recorder { return p },
		func(uow application.UnitOfWork, rec resolvepending.Recorder, policy application.PendingPolicy, log *slog.Logger) *resolvepending.UseCase {
			return resolvepending.New(resolvepending.Deps{UnitOfWork: uow, Recorder: rec, Logger: log, Now: time.Now, NewID: uuid.New, Policy: policy})
		},
	),
	fx.Provide(
		func(reg *prometheus.Registry) *promrec.Reconcile { return promrec.NewReconcile(reg) },
		func(r *promrec.Reconcile) reconcile.Recorder { return r },
		func(auditor application.LedgerAuditor, rec reconcile.Recorder, log *slog.Logger) *reconcile.UseCase {
			return reconcile.New(reconcile.Deps{Auditor: auditor, Recorder: rec, Logger: log})
		},
	),
	fx.Invoke(func(*openwallet.UseCase, *processwager.UseCase, *resolvepending.UseCase, *reconcile.UseCase) {}),
)
