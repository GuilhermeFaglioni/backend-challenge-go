// Package logger builds the structured JSON logger shared by the application.
package logger

import (
	"log/slog"
	"os"

	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

var Module = fx.Module("logger", fx.Provide(New))

func New(cfg config.Config) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, err
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler).With(
		slog.String("service", "wagering"),
		slog.String("instanceId", cfg.InstanceID),
	), nil
}
