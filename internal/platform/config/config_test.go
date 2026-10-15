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
		SQS:             SQS{InputQueue: "wager-transactions.fifo", EventsQueue: "wager-events.fifo"},
		OIDC: OIDC{
			IssuerURL:    "http://localhost:8180/realms/wagering",
			DiscoveryURL: "http://keycloak:8080/realms/wagering",
			Audience:     "wallet-api",
		},
		Outbox: Outbox{PollInterval: 500 * time.Millisecond, BatchSize: 10, Lease: 30 * time.Second},
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
