package config

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		InstanceID:      "test",
		HTTPAddr:        ":8080",
		LogLevel:        "info",
		ShutdownTimeout: 30 * time.Second,
		Database:        Database{URL: "postgres://localhost/wagering", MaxConns: 10},
		AWS:             AWS{Region: "us-east-1", EndpointURL: "http://localhost:4566"},
		SQS: SQS{
			InputQueue: "wager-transactions.fifo", EventsQueue: "wager-events.fifo", DeadLetterQueue: "wager-transactions-dlq.fifo",
			Consumer: Consumer{Workers: 4, BatchSize: 10, PollWait: 20 * time.Second, ProcessingTimeout: 20 * time.Second, RetryBackoffBase: 2 * time.Second, RetryBackoffMax: time.Minute},
		},
		OIDC: OIDC{
			IssuerURL:    "http://localhost:8180/realms/wagering",
			DiscoveryURL: "http://keycloak:8080/realms/wagering",
			Audience:     "wallet-api",
		},
		Outbox: Outbox{PollInterval: 500 * time.Millisecond, BatchSize: 10, Lease: 30 * time.Second},
		PendingReference: PendingReference{
			MaxAttempts: 10, TTL: 15 * time.Minute, BackoffBase: 2 * time.Second, BackoffMax: time.Minute,
			PollInterval: 500 * time.Millisecond, BatchSize: 10, Lease: 30 * time.Second,
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "invalid log level", mutate: func(c *Config) { c.LogLevel = "loud" }, wantErr: "LOG_LEVEL"},
		{name: "zero shutdown timeout", mutate: func(c *Config) { c.ShutdownTimeout = 0 }, wantErr: "SHUTDOWN_TIMEOUT"},
		{name: "zero max conns", mutate: func(c *Config) { c.Database.MaxConns = 0 }, wantErr: "DATABASE_MAX_CONNS"},
		{name: "non fifo input queue", mutate: func(c *Config) { c.SQS.InputQueue = "wager-transactions" }, wantErr: "SQS_INPUT_QUEUE"},
		{name: "non fifo dead letter queue", mutate: func(c *Config) { c.SQS.DeadLetterQueue = "wager-transactions-dlq" }, wantErr: "SQS_DEAD_LETTER_QUEUE"},
		{name: "no consumer workers", mutate: func(c *Config) { c.SQS.Consumer.Workers = 0 }, wantErr: "SQS_CONSUMER_WORKERS"},
		{name: "consumer batch beyond SQS", mutate: func(c *Config) { c.SQS.Consumer.BatchSize = 11 }, wantErr: "SQS_CONSUMER_BATCH_SIZE"},
		{name: "consumer batch of none", mutate: func(c *Config) { c.SQS.Consumer.BatchSize = 0 }, wantErr: "SQS_CONSUMER_BATCH_SIZE"},
		{name: "consumer batch at the limit", mutate: func(c *Config) { c.SQS.Consumer.BatchSize = 10 }},
		{name: "poll wait beyond SQS", mutate: func(c *Config) { c.SQS.Consumer.PollWait = 21 * time.Second }, wantErr: "SQS_CONSUMER_POLL_WAIT"},
		{name: "negative poll wait", mutate: func(c *Config) { c.SQS.Consumer.PollWait = -time.Second }, wantErr: "SQS_CONSUMER_POLL_WAIT"},
		{name: "short poll", mutate: func(c *Config) { c.SQS.Consumer.PollWait = 0 }},
		{name: "no processing timeout", mutate: func(c *Config) { c.SQS.Consumer.ProcessingTimeout = 0 }, wantErr: "SQS_CONSUMER_PROCESSING_TIMEOUT"},
		{name: "backoff base under a second", mutate: func(c *Config) { c.SQS.Consumer.RetryBackoffBase = 500 * time.Millisecond }, wantErr: "SQS_CONSUMER_RETRY_BACKOFF_BASE"},
		{name: "backoff max under the base", mutate: func(c *Config) { c.SQS.Consumer.RetryBackoffMax = time.Second }, wantErr: "SQS_CONSUMER_RETRY_BACKOFF_MAX"},
		{name: "no pending attempts", mutate: func(c *Config) { c.PendingReference.MaxAttempts = 0 }, wantErr: "PENDING_REFERENCE_MAX_ATTEMPTS"},
		{name: "one pending attempt", mutate: func(c *Config) { c.PendingReference.MaxAttempts = 1 }},
		{name: "no pending ttl", mutate: func(c *Config) { c.PendingReference.TTL = 0 }, wantErr: "PENDING_REFERENCE_TTL"},
		{name: "negative pending ttl", mutate: func(c *Config) { c.PendingReference.TTL = -time.Minute }, wantErr: "PENDING_REFERENCE_TTL"},
		{name: "no pending backoff base", mutate: func(c *Config) { c.PendingReference.BackoffBase = 0 }, wantErr: "PENDING_REFERENCE_BACKOFF_BASE"},
		{name: "pending backoff max under the base", mutate: func(c *Config) { c.PendingReference.BackoffMax = time.Second }, wantErr: "PENDING_REFERENCE_BACKOFF_MAX"},
		{name: "pending backoff max equal to the base", mutate: func(c *Config) { c.PendingReference.BackoffMax = 2 * time.Second }},
		{name: "no pending poll interval", mutate: func(c *Config) { c.PendingReference.PollInterval = 0 }, wantErr: "PENDING_REFERENCE_POLL_INTERVAL"},
		{name: "no pending batch", mutate: func(c *Config) { c.PendingReference.BatchSize = 0 }, wantErr: "PENDING_REFERENCE_BATCH_SIZE"},
		{name: "pending lease too short", mutate: func(c *Config) { c.PendingReference.Lease = 500 * time.Millisecond }, wantErr: "PENDING_REFERENCE_LEASE"},
		{name: "relative issuer", mutate: func(c *Config) { c.OIDC.IssuerURL = "/realms/wagering" }, wantErr: "OIDC_ISSUER_URL"},
		{name: "bad aws endpoint", mutate: func(c *Config) { c.AWS.EndpointURL = "localhost:4566" }, wantErr: "AWS_ENDPOINT_URL"},
		{name: "empty audience", mutate: func(c *Config) { c.OIDC.Audience = "" }, wantErr: "OIDC_AUDIENCE"},
		{name: "zero outbox poll interval", mutate: func(c *Config) { c.Outbox.PollInterval = 0 }, wantErr: "OUTBOX_POLL_INTERVAL"},
		{name: "zero outbox batch", mutate: func(c *Config) { c.Outbox.BatchSize = 0 }, wantErr: "OUTBOX_BATCH_SIZE"},
		{name: "outbox batch beyond SQS", mutate: func(c *Config) { c.Outbox.BatchSize = 11 }, wantErr: "OUTBOX_BATCH_SIZE"},
		{name: "outbox batch at the limit", mutate: func(c *Config) { c.Outbox.BatchSize = 10 }},
		{name: "outbox lease too short", mutate: func(c *Config) { c.Outbox.Lease = 500 * time.Millisecond }, wantErr: "OUTBOX_LEASE"},
		{name: "cors origin ok", mutate: func(c *Config) { c.CORSAllowedOrigins = []string{"http://localhost:8089", "https://docs.example.com"} }},
		{name: "cors wildcard", mutate: func(c *Config) { c.CORSAllowedOrigins = []string{"*"} }, wantErr: "CORS_ALLOWED_ORIGINS"},
		{name: "cors with a path", mutate: func(c *Config) { c.CORSAllowedOrigins = []string{"http://localhost:8089/ui"} }, wantErr: "CORS_ALLOWED_ORIGINS"},
		{name: "cors with a trailing slash", mutate: func(c *Config) { c.CORSAllowedOrigins = []string{"http://localhost:8089/"} }, wantErr: "CORS_ALLOWED_ORIGINS"},
		{name: "cors without a scheme", mutate: func(c *Config) { c.CORSAllowedOrigins = []string{"localhost:8089"} }, wantErr: "CORS_ALLOWED_ORIGINS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error mentioning %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadDefaultsDiscoveryToIssuer(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/wagering")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OIDC.DiscoveryURL != cfg.OIDC.IssuerURL {
		t.Fatalf("expected discovery URL to default to issuer, got %q", cfg.OIDC.DiscoveryURL)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")

	if _, err := Load(); err == nil {
		t.Fatal("expected error for missing DATABASE_URL")
	}
}

func TestLoadParsesTheCORSOrigins(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/wagering")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")
	t.Setenv("CORS_ALLOWED_ORIGINS", "http://localhost:8089,https://docs.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CORSAllowedOrigins) != 2 || cfg.CORSAllowedOrigins[0] != "http://localhost:8089" || cfg.CORSAllowedOrigins[1] != "https://docs.example.com" {
		t.Fatalf("got %v", cfg.CORSAllowedOrigins)
	}
}

func TestNoCORSOriginsByDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/wagering")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CORSAllowedOrigins) != 0 {
		t.Fatalf("got %v", cfg.CORSAllowedOrigins)
	}
}

func TestOutboxDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/wagering")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Outbox{PollInterval: 500 * time.Millisecond, BatchSize: 10, Lease: 30 * time.Second}
	if cfg.Outbox != want {
		t.Fatalf("got %+v, want %+v", cfg.Outbox, want)
	}
}

func TestConsumerDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/wagering")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQS.DeadLetterQueue != "wager-transactions-dlq.fifo" {
		t.Fatalf("dead letter queue %q", cfg.SQS.DeadLetterQueue)
	}
	want := validConfig().SQS.Consumer
	if cfg.SQS.Consumer != want {
		t.Fatalf("got %+v, want %+v", cfg.SQS.Consumer, want)
	}
}

func TestConsumerSettingsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/wagering")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")
	t.Setenv("SQS_DEAD_LETTER_QUEUE", "other-dlq.fifo")
	t.Setenv("SQS_CONSUMER_WORKERS", "7")
	t.Setenv("SQS_CONSUMER_BATCH_SIZE", "3")
	t.Setenv("SQS_CONSUMER_POLL_WAIT", "1s")
	t.Setenv("SQS_CONSUMER_PROCESSING_TIMEOUT", "5s")
	t.Setenv("SQS_CONSUMER_RETRY_BACKOFF_BASE", "3s")
	t.Setenv("SQS_CONSUMER_RETRY_BACKOFF_MAX", "9s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Consumer{Workers: 7, BatchSize: 3, PollWait: time.Second, ProcessingTimeout: 5 * time.Second, RetryBackoffBase: 3 * time.Second, RetryBackoffMax: 9 * time.Second}
	if cfg.SQS.Consumer != want || cfg.SQS.DeadLetterQueue != "other-dlq.fifo" {
		t.Fatalf("got %+v %q", cfg.SQS.Consumer, cfg.SQS.DeadLetterQueue)
	}
}

func TestPendingReferenceDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/wagering")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8180/realms/wagering")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PendingReference != validConfig().PendingReference {
		t.Fatalf("defaults: got %+v", cfg.PendingReference)
	}

	t.Setenv("PENDING_REFERENCE_MAX_ATTEMPTS", "3")
	t.Setenv("PENDING_REFERENCE_TTL", "2m")
	t.Setenv("PENDING_REFERENCE_BACKOFF_BASE", "1s")
	t.Setenv("PENDING_REFERENCE_BACKOFF_MAX", "5s")
	t.Setenv("PENDING_REFERENCE_POLL_INTERVAL", "100ms")
	t.Setenv("PENDING_REFERENCE_BATCH_SIZE", "4")
	t.Setenv("PENDING_REFERENCE_LEASE", "3s")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	want := PendingReference{MaxAttempts: 3, TTL: 2 * time.Minute, BackoffBase: time.Second, BackoffMax: 5 * time.Second, PollInterval: 100 * time.Millisecond, BatchSize: 4, Lease: 3 * time.Second}
	if cfg.PendingReference != want {
		t.Fatalf("got %+v, want %+v", cfg.PendingReference, want)
	}
}
