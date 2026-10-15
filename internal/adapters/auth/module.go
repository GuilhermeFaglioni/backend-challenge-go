package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// startRetryEvery is how long to wait between attempts to reach the identity
// provider while the application starts.
const startRetryEvery = time.Second

// Module provides the token verifier. It is started before anything that uses it
// (the HTTP server), and a service that cannot validate tokens does not start.
var Module = fx.Module("auth", fx.Provide(NewFromConfig))

// NewFromConfig builds the verifier from the configuration and ties it to the Fx
// lifecycle: on start it discovers the identity provider, retrying until the start
// deadline (the provider may still be booting next to us); on stop it closes its
// connections.
func NewFromConfig(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*Verifier, error) {
	v, err := New(Options{
		Issuer:       cfg.OIDC.IssuerURL,
		DiscoveryURL: cfg.OIDC.DiscoveryURL,
		Audience:     cfg.OIDC.Audience,
		Logger:       log,
	})
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			for {
				err := v.Start(ctx)
				if err == nil {
					log.InfoContext(ctx, "identity provider ready", slog.String("issuer", cfg.OIDC.IssuerURL))
					return nil
				}
				if errors.Is(err, ErrMisconfigured) {
					return err // waiting cannot fix a wrong issuer
				}
				log.WarnContext(ctx, "waiting for the identity provider", slog.Any("error", err))
				select {
				case <-ctx.Done():
					return fmt.Errorf("the identity provider is not reachable: %w", err)
				case <-time.After(startRetryEvery):
				}
			}
		},
		OnStop: func(ctx context.Context) error {
			v.Close()
			log.InfoContext(ctx, "identity provider client closed")
			return nil
		},
	})
	return v, nil
}
