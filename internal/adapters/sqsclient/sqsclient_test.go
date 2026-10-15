package sqsclient

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/fx/fxtest"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

func TestEachComponentHoldsItsOwnCredentials(t *testing.T) {
	cfg := config.Config{AWS: config.AWS{Region: "us-east-1", EndpointURL: "http://localhost:4566"}}
	cfg.SQS.Consumer.Credentials = config.Credentials{AccessKeyID: "consumer-key", SecretAccessKey: "consumer-secret"}
	cfg.SQS.Publisher.Credentials = config.Credentials{AccessKeyID: "publisher-key", SecretAccessKey: "publisher-secret"}
	lc := fxtest.NewLifecycle(t)

	consumer, publisher, err := New(lc, cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	c, err := consumer.Options().Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "consumer-key", c.AccessKeyID)
	require.Equal(t, "consumer-secret", c.SecretAccessKey)
	p, err := publisher.Options().Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "publisher-key", p.AccessKeyID, "the publisher does not borrow the consumer's key")
	require.Equal(t, "http://localhost:4566", *consumer.Options().BaseEndpoint)
	lc.RequireStart().RequireStop()
}

func TestWithoutCredentialsOfItsOwnAComponentUsesTheDefaultChain(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "from-the-environment")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
	cfg := config.Config{AWS: config.AWS{Region: "us-east-1"}}
	cfg.SQS.Publisher.Credentials = config.Credentials{AccessKeyID: "publisher-key", SecretAccessKey: "publisher-secret"}

	consumer, publisher, err := New(fxtest.NewLifecycle(t), cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	c, err := consumer.Options().Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "from-the-environment", c.AccessKeyID)
	p, err := publisher.Options().Credentials.Retrieve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "publisher-key", p.AccessKeyID)
}
