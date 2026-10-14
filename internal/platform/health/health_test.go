package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func newRegistry(checks ...Check) *Registry {
	return NewRegistry(Params{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Checks: checks})
}

func TestReadyAllChecksPass(t *testing.T) {
	r := newRegistry(Check{Name: "postgres", Fn: func(context.Context) error { return nil }})

	report, ok := r.Ready(context.Background())
	if !ok || report.Status != "ok" || report.Checks["postgres"] != "ok" {
		t.Fatalf("expected ready, got ok=%v report=%+v", ok, report)
	}
}

func TestReadyFailingCheckHidesErrorDetails(t *testing.T) {
	r := newRegistry(
		Check{Name: "postgres", Fn: func(context.Context) error { return nil }},
		Check{Name: "sqs", Fn: func(context.Context) error { return errors.New("dial tcp 10.0.0.5:4566: refused") }},
	)

	report, ok := r.Ready(context.Background())
	if ok || report.Status != "unavailable" {
		t.Fatalf("expected not ready, got ok=%v report=%+v", ok, report)
	}
	if report.Checks["sqs"] != "unavailable" {
		t.Fatalf("expected sqs to be reported unavailable without details, got %q", report.Checks["sqs"])
	}
}

func TestReadyFailsWhileDraining(t *testing.T) {
	r := newRegistry()
	r.MarkDraining()

	report, ok := r.Ready(context.Background())
	if ok || report.Checks["shutdown"] != "draining" {
		t.Fatalf("expected draining to fail readiness, got ok=%v report=%+v", ok, report)
	}
}
