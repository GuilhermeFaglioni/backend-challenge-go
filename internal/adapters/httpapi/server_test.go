package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx/fxtest"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

func newTestMux(checks ...health.Check) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := prometheus.NewRegistry()
	routes := NewRoutes(fakeAuthenticator{}, logger, metrics)
	registerRoutes(routes, health.NewRegistry(health.Params{Logger: logger, Checks: checks}), metrics, NewWallets(&fakeOpener{}, &fakeReader{}, logger))
	return routes.Handler()
}

func TestLive(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestMux().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestReadyReturns503WhenACheckFails(t *testing.T) {
	failing := health.Check{Name: "postgres", Fn: func(context.Context) error { return errors.New("down") }}
	rec := httptest.NewRecorder()
	newTestMux(failing).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health/ready", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var report health.Report
	if err := json.NewDecoder(rec.Body).Decode(&report); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if report.Checks["postgres"] != "unavailable" {
		t.Fatalf("expected postgres unavailable, got %+v", report)
	}
}

func TestUnknownMethodIsRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestMux().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/health/live", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// The origins in the configuration reach the server that is really served.
func TestNewServerAppliesTheConfiguredCORSOrigins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(ServerParams{
		Lifecycle: fxtest.NewLifecycle(t),
		Config:    config.Config{HTTPAddr: "127.0.0.1:0", CORSAllowedOrigins: []string{"http://localhost:8089"}},
		Logger:    logger,
		Health:    health.NewRegistry(health.Params{Logger: logger}),
		Metrics:   prometheus.NewRegistry(),
		Authn:     fakeAuthenticator{},
		Wallets:   NewWallets(&fakeOpener{}, &fakeReader{}, logger),
	})

	preflight := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/wallets", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := preflight("http://localhost:8089"); rec.Code != http.StatusNoContent {
		t.Fatalf("an allowed origin: expected 204, got %d", rec.Code)
	}
	if rec := preflight("http://evil.example"); rec.Code == http.StatusNoContent {
		t.Fatal("an origin that is not configured got its preflight approved")
	}
}
