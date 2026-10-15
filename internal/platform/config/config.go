// Package config loads and validates the application configuration from
// environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	InstanceID      string        `env:"INSTANCE_ID" envDefault:"local"`
	HTTPAddr        string        `env:"HTTP_ADDR" envDefault:":8080"`
	LogLevel        string        `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"30s"`
	// CORSAllowedOrigins are the browser origins (scheme://host[:port]) allowed to
	// call the API, for the Swagger UI. Empty, the default, allows none.
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envSeparator:","`

	Database Database `envPrefix:"DATABASE_"`
	AWS      AWS      `envPrefix:"AWS_"`
	SQS      SQS      `envPrefix:"SQS_"`
	OIDC     OIDC     `envPrefix:"OIDC_"`
}

type Database struct {
	URL      string `env:"URL,required,notEmpty"`
	MaxConns int32  `env:"MAX_CONNS" envDefault:"10"`
}

type AWS struct {
	Region string `env:"REGION,required,notEmpty"`
	// EndpointURL points the SDK at LocalStack; empty means the real AWS endpoint.
	EndpointURL string `env:"ENDPOINT_URL"`
}

type SQS struct {
	InputQueue  string `env:"INPUT_QUEUE" envDefault:"wager-transactions.fifo"`
	EventsQueue string `env:"EVENTS_QUEUE" envDefault:"wager-events.fifo"`
}

type OIDC struct {
	// IssuerURL is the expected `iss` claim of incoming tokens.
	IssuerURL string `env:"ISSUER_URL,required,notEmpty"`
	// DiscoveryURL is where the service fetches the OIDC metadata and JWKS.
	// It differs from IssuerURL inside Docker, where Keycloak is reached by
	// its service name but issues tokens for the host-facing URL.
	DiscoveryURL string `env:"DISCOVERY_URL"`
	Audience     string `env:"AUDIENCE" envDefault:"wallet-api"`
}

// Load parses the environment and validates the result.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, err
	}
	if cfg.OIDC.DiscoveryURL == "" {
		cfg.OIDC.DiscoveryURL = cfg.OIDC.IssuerURL
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error

	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("SHUTDOWN_TIMEOUT must be positive"))
	}
	if c.Database.MaxConns <= 0 {
		errs = append(errs, errors.New("DATABASE_MAX_CONNS must be positive"))
	}
	if c.AWS.EndpointURL != "" {
		if err := validateHTTPURL(c.AWS.EndpointURL); err != nil {
			errs = append(errs, fmt.Errorf("AWS_ENDPOINT_URL: %w", err))
		}
	}
	for name, queue := range map[string]string{"SQS_INPUT_QUEUE": c.SQS.InputQueue, "SQS_EVENTS_QUEUE": c.SQS.EventsQueue} {
		if !strings.HasSuffix(queue, ".fifo") {
			errs = append(errs, fmt.Errorf("%s must name a FIFO queue (suffix .fifo), got %q", name, queue))
		}
	}
	if err := validateHTTPURL(c.OIDC.IssuerURL); err != nil {
		errs = append(errs, fmt.Errorf("OIDC_ISSUER_URL: %w", err))
	}
	if err := validateHTTPURL(c.OIDC.DiscoveryURL); err != nil {
		errs = append(errs, fmt.Errorf("OIDC_DISCOVERY_URL: %w", err))
	}
	if c.OIDC.Audience == "" {
		errs = append(errs, errors.New("OIDC_AUDIENCE must not be empty"))
	}
	for _, origin := range c.CORSAllowedOrigins {
		if err := validateOrigin(origin); err != nil {
			errs = append(errs, fmt.Errorf("CORS_ALLOWED_ORIGINS: %q: %w", origin, err))
		}
	}

	return errors.Join(errs...)
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL, got %q", raw)
	}
	return nil
}

// validateOrigin accepts only a bare origin: scheme, host and optional port. A
// wildcard, a path or a trailing slash would silently never match (or match too
// much), so they are refused at start-up.
func validateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be scheme://host[:port], with http or https")
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("must have no path, query, fragment or user info")
	}
	return nil
}
