// Package sqsclient builds the AWS SQS client shared by the consumer and the
// outbox publisher. Locally it points at LocalStack through AWS_ENDPOINT_URL.
package sqsclient

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

var Module = fx.Module("sqsclient",
	fx.Provide(
		New,
		fx.Annotate(newReadinessCheck, fx.ResultTags(`group:"readiness"`)),
	),
)

// New builds the client. Credentials come from the SDK default chain
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY locally).
//
// The HTTP transport is created here, not by the SDK, because it keeps idle
// connections (and their goroutines) open and must be closed on stop so that
// nothing outlives the application. The SDK's own client hides its transport
// (BuildableClient.GetTransport returns a copy), so it cannot be closed. The
// transport starts from the SDK's defaults (connection pool size, TLS minimum,
// timeouts) rather than from net/http's.
func New(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (*sqs.Client, error) {
	transport := awshttp.NewBuildableClient().GetTransport()
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.AWS.Region),
		// No client-level timeout: SQS long polling holds requests open, and
		// callers bound every call with a context deadline.
		awsconfig.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			transport.CloseIdleConnections()
			log.InfoContext(ctx, "sqs client closed")
			return nil
		},
	})

	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.AWS.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.AWS.EndpointURL)
		}
	}), nil
}

// newReadinessCheck reports SQS as ready when the input queue is resolvable.
func newReadinessCheck(client *sqs.Client, cfg config.Config) health.Check {
	return health.Check{
		Name: "sqs",
		Fn: func(ctx context.Context) error {
			_, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(cfg.SQS.InputQueue)})
			return err
		},
	}
}
