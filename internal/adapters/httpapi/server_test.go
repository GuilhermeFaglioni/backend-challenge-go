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

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

func newTestMux(checks ...health.Check) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := prometheus.NewRegistry()
	routes := NewRoutes(fakeAuthenticator{}, logger, metrics)
	registerRoutes(routes, health.NewRegistry(health.Params{Logger: logger, Checks: checks}), metrics)
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
