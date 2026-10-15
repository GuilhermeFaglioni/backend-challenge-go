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
	Outbox   Outbox   `envPrefix:"OUTBOX_"`
	// PendingReference tunes the wait of an operation for a Reference that has not arrived.
	PendingReference PendingReference `envPrefix:"PENDING_REFERENCE_"`
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
	// DeadLetterQueue is where the consumer sends the messages it can never handle
	// (the queue's own redrive sends the ones whose attempts ran out).
	DeadLetterQueue string   `env:"DEAD_LETTER_QUEUE" envDefault:"wager-transactions-dlq.fifo"`
	Consumer        Consumer `envPrefix:"CONSUMER_"`
}

// Consumer tunes the loop that reads the input queue.
type Consumer struct {
	// Workers is how many receive loops run in each instance.
	Workers int `env:"WORKERS" envDefault:"4"`
	// BatchSize is how many messages one receive asks for (SQS allows 1 to 10).
	BatchSize int `env:"BATCH_SIZE" envDefault:"10"`
	// PollWait is the long-poll wait of a receive; SQS allows up to 20s.
	PollWait time.Duration `env:"POLL_WAIT" envDefault:"20s"`
	// ProcessingTimeout bounds the handling of one message. It must be shorter than
	// the queue's visibility timeout (30s), or the message could be handed to another
	// instance while this one is still working on it.
	ProcessingTimeout time.Duration `env:"PROCESSING_TIMEOUT" envDefault:"20s"`
	// RetryBackoffBase is how long a message that failed for a transient reason stays
	// invisible after its first failure; it doubles with each receive.
	RetryBackoffBase time.Duration `env:"RETRY_BACKOFF_BASE" envDefault:"2s"`
	// RetryBackoffMax is the longest that wait grows to.
	RetryBackoffMax time.Duration `env:"RETRY_BACKOFF_MAX" envDefault:"60s"`
}

// Outbox tunes the publisher that sends committed events to the events queue.
type Outbox struct {
	// PollInterval is how long the publisher waits when it found nothing to send.
	PollInterval time.Duration `env:"POLL_INTERVAL" envDefault:"500ms"`
	// BatchSize is how many events are reserved and sent together; SQS accepts at
	// most 10 in one batch.
	BatchSize int `env:"BATCH_SIZE" envDefault:"10"`
	// Lease is how long a reserved event stays reserved. If the instance that took
	// it dies, another one takes it after this time.
	Lease time.Duration `env:"LEASE" envDefault:"30s"`
}

// PendingReference is how an operation waits for its Reference and how the worker that
// takes it up again behaves.
type PendingReference struct {
	// MaxAttempts is how many times an operation is tried, the first, when it arrives,
	// included; it is refused when the last one finds its Reference still missing.
	MaxAttempts int `env:"MAX_ATTEMPTS" envDefault:"10"`
	// TTL is how long after it arrived an operation may still wait.
	TTL time.Duration `env:"TTL" envDefault:"15m"`
	// BackoffBase is the gap after the first attempt; it doubles with each attempt.
	BackoffBase time.Duration `env:"BACKOFF_BASE" envDefault:"2s"`
	// BackoffMax is the longest the gap grows to.
	BackoffMax time.Duration `env:"BACKOFF_MAX" envDefault:"60s"`
	// PollInterval is how long the worker waits when nothing is due.
	PollInterval time.Duration `env:"POLL_INTERVAL" envDefault:"500ms"`
	// BatchSize is how many due operations a worker reserves at once.
	BatchSize int `env:"BATCH_SIZE" envDefault:"10"`
	// Lease is how long a reserved operation stays reserved. If the instance that took it
	// dies, another one takes it after this time.
	Lease time.Duration `env:"LEASE" envDefault:"30s"`
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

// maxOutboxBatch is the most entries SQS takes in one SendMessageBatch.
const maxOutboxBatch = 10

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
	for name, queue := range map[string]string{
		"SQS_INPUT_QUEUE": c.SQS.InputQueue, "SQS_EVENTS_QUEUE": c.SQS.EventsQueue, "SQS_DEAD_LETTER_QUEUE": c.SQS.DeadLetterQueue,
	} {
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
	if c.Outbox.PollInterval <= 0 {
		errs = append(errs, errors.New("OUTBOX_POLL_INTERVAL must be positive"))
	}
	if c.Outbox.BatchSize < 1 || c.Outbox.BatchSize > maxOutboxBatch {
		errs = append(errs, fmt.Errorf("OUTBOX_BATCH_SIZE must be between 1 and %d (the SQS batch limit)", maxOutboxBatch))
	}
	if c.Outbox.Lease < time.Second {
		errs = append(errs, errors.New("OUTBOX_LEASE must be at least 1s"))
	}
	errs = append(errs, c.SQS.Consumer.validate()...)
	errs = append(errs, c.PendingReference.validate()...)
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

// maxReceiveBatch is the most messages SQS returns from one receive, and maxPollWait
// the longest long poll it allows.
const (
	maxReceiveBatch = 10
	maxPollWait     = 20 * time.Second
)

func (c Consumer) validate() []error {
	var errs []error
	if c.Workers < 1 {
		errs = append(errs, errors.New("SQS_CONSUMER_WORKERS must be at least 1"))
	}
	if c.BatchSize < 1 || c.BatchSize > maxReceiveBatch {
		errs = append(errs, fmt.Errorf("SQS_CONSUMER_BATCH_SIZE must be between 1 and %d (the SQS receive limit)", maxReceiveBatch))
	}
	if c.PollWait < 0 || c.PollWait > maxPollWait {
		errs = append(errs, fmt.Errorf("SQS_CONSUMER_POLL_WAIT must be between 0 and %s (the SQS long poll limit)", maxPollWait))
	}
	if c.ProcessingTimeout <= 0 {
		errs = append(errs, errors.New("SQS_CONSUMER_PROCESSING_TIMEOUT must be positive"))
	}
	if c.RetryBackoffBase < time.Second {
		errs = append(errs, errors.New("SQS_CONSUMER_RETRY_BACKOFF_BASE must be at least 1s (SQS visibility has a resolution of seconds)"))
	}
	if c.RetryBackoffMax < c.RetryBackoffBase {
		errs = append(errs, errors.New("SQS_CONSUMER_RETRY_BACKOFF_MAX must not be less than SQS_CONSUMER_RETRY_BACKOFF_BASE"))
	}
	return errs
}

func (p PendingReference) validate() []error {
	var errs []error
	if p.MaxAttempts < 1 {
		errs = append(errs, errors.New("PENDING_REFERENCE_MAX_ATTEMPTS must be at least 1"))
	}
	if p.TTL <= 0 {
		errs = append(errs, errors.New("PENDING_REFERENCE_TTL must be positive"))
	}
	if p.BackoffBase <= 0 {
		errs = append(errs, errors.New("PENDING_REFERENCE_BACKOFF_BASE must be positive"))
	}
	if p.BackoffMax < p.BackoffBase {
		errs = append(errs, errors.New("PENDING_REFERENCE_BACKOFF_MAX must not be less than PENDING_REFERENCE_BACKOFF_BASE"))
	}
	if p.PollInterval <= 0 {
		errs = append(errs, errors.New("PENDING_REFERENCE_POLL_INTERVAL must be positive"))
	}
	if p.BatchSize < 1 {
		errs = append(errs, errors.New("PENDING_REFERENCE_BATCH_SIZE must be at least 1"))
	}
	if p.Lease < time.Second {
		errs = append(errs, errors.New("PENDING_REFERENCE_LEASE must be at least 1s"))
	}
	return errs
}
