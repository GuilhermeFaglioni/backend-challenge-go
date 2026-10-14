// Package health aggregates readiness checks contributed by the adapters.
//
// Adapters provide a Check into the "readiness" Fx value group; the Registry
// runs all of them for GET /health/ready.
package health

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"go.uber.org/fx"
)

var Module = fx.Module("health", fx.Provide(NewRegistry))

const checkTimeout = 2 * time.Second

type Check struct {
	Name string
	Fn   func(context.Context) error
}

type Params struct {
	fx.In

	Logger *slog.Logger
	Checks []Check `group:"readiness"`
}

type Registry struct {
	log      *slog.Logger
	checks   []Check
	draining atomic.Bool
}

type Report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func NewRegistry(p Params) *Registry {
	return &Registry{log: p.Logger, checks: p.Checks}
}

// MarkDraining makes readiness fail from now on, so the instance leaves the
// load balancer before it stops accepting work.
func (r *Registry) MarkDraining() {
	r.draining.Store(true)
}

// Ready runs every check. Error details are logged, never returned, so the
// public endpoint does not leak infrastructure information.
func (r *Registry) Ready(ctx context.Context) (Report, bool) {
	report := Report{Status: "ok", Checks: make(map[string]string, len(r.checks)+1)}
	ok := true

	if r.draining.Load() {
		ok = false
		report.Checks["shutdown"] = "draining"
	}
	for _, c := range r.checks {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		err := c.Fn(checkCtx)
		cancel()
		if err != nil {
			ok = false
			report.Checks[c.Name] = "unavailable"
			r.log.WarnContext(ctx, "readiness check failed", slog.String("check", c.Name), slog.Any("error", err))
			continue
		}
		report.Checks[c.Name] = "ok"
	}

	if !ok {
		report.Status = "unavailable"
	}
	return report, ok
}
