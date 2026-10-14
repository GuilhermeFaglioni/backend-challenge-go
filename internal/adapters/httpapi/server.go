// Package httpapi is the HTTP adapter: routing, handlers and the server
// lifecycle. Business endpoints are added as the use cases land.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

var Module = fx.Module("httpapi",
	fx.Provide(NewServer),
	fx.Invoke(func(*http.Server) {}),
)

type ServerParams struct {
	fx.In

	Lifecycle  fx.Lifecycle
	Shutdowner fx.Shutdowner
	Config     config.Config
	Logger     *slog.Logger
	Health     *health.Registry
	Metrics    *prometheus.Registry
}

func NewServer(p ServerParams) *http.Server {
	srv := &http.Server{
		Addr:              p.Config.HTTPAddr,
		Handler:           newMux(p.Health, p.Metrics),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(p.Logger.Handler(), slog.LevelError),
	}

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Listen synchronously so a busy port fails the startup instead of
			// a background goroutine.
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
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

func newMux(hr *health.Registry, reg *prometheus.Registry) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		report, ok := hr.Ready(r.Context())
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, report)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
