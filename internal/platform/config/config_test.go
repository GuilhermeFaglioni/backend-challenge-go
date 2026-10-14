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
