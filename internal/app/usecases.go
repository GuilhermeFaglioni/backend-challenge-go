package app

import (
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/promrec"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
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
	fx.Invoke(func(*openwallet.UseCase) {}),
)
