package app

import (
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// TestGraphIsComplete checks that every constructor's dependencies are
// provided, without opening connections. Start and stop against real
// infrastructure are covered by the integration tests.
func TestGraphIsComplete(t *testing.T) {
	cfg := config.Config{
		InstanceID:      "test",
		HTTPAddr:        ":0",
		LogLevel:        "info",
		ShutdownTimeout: 5 * time.Second,
		Database:        config.Database{URL: "postgres://localhost/wagering", MaxConns: 2},
		AWS:             config.AWS{Region: "us-east-1", EndpointURL: "http://localhost:4566"},
		SQS:             config.SQS{InputQueue: "wager-transactions.fifo", EventsQueue: "wager-events.fifo"},
		OIDC: config.OIDC{
			IssuerURL:    "http://localhost:8180/realms/wagering",
			DiscoveryURL: "http://localhost:8180/realms/wagering",
			Audience:     "wallet-api",
		},
	}

	if err := fx.ValidateApp(New(cfg)); err != nil {
		t.Fatalf("invalid fx graph: %v", err)
	}
}
