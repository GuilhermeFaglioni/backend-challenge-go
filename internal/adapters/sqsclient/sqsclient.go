// Package sqsclient builds the AWS SQS clients of the consumer and of the outbox
// publisher. Locally they point at LocalStack through AWS_ENDPOINT_URL.
//
// There are two clients, one per component, so that each can hold credentials of its own,
// with a policy that allows it only what it does (deploy/aws/iam): the consumer reads the
// input queue and writes the dead-letter queue, the publisher writes the events queue.
// Without credentials of its own a client uses the SDK's default chain.
package sqsclient

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
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

// Consumer is the client of the queue consumer.
type Consumer struct{ *sqs.Client }

// Publisher is the client of the outbox publisher.
type Publisher struct{ *sqs.Client }

// New builds the two clients.
//
// The HTTP transport is created here, not by the SDK, because it keeps idle
// connections (and their goroutines) open and must be closed on stop so that
// nothing outlives the application. The SDK's own client hides its transport
// (BuildableClient.GetTransport returns a copy), so it cannot be closed. The
// transport starts from the SDK's defaults (connection pool size, TLS minimum,
// timeouts) rather than from net/http's. Both clients share it.
func New(lc fx.Lifecycle, cfg config.Config, log *slog.Logger) (Consumer, Publisher, error) {
	transport := awshttp.NewBuildableClient().GetTransport()
	httpClient := &http.Client{Transport: transport} // no client-level timeout: long polling holds requests open, and callers bound every call with a context deadline

	consumer, err := newClient(cfg, httpClient, cfg.SQS.Consumer.Credentials)
	if err != nil {
		return Consumer{}, Publisher{}, fmt.Errorf("the consumer's sqs client: %w", err)
	}
	publisher, err := newClient(cfg, httpClient, cfg.SQS.Publisher.Credentials)
	if err != nil {
		return Consumer{}, Publisher{}, fmt.Errorf("the publisher's sqs client: %w", err)
	}

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			transport.CloseIdleConnections()
			log.InfoContext(ctx, "sqs client closed")
			return nil
		},
	})
	return Consumer{consumer}, Publisher{publisher}, nil
}

func newClient(cfg config.Config, httpClient *http.Client, own config.Credentials) (*sqs.Client, error) {
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.AWS.Region),
		awsconfig.WithHTTPClient(httpClient),
	}
	if own.AccessKeyID != "" {
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(own.AccessKeyID, own.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), options...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.AWS.EndpointURL != "" {
			o.BaseEndpoint = aws.String(cfg.AWS.EndpointURL)
		}
	}), nil
}

// newReadinessCheck reports SQS as ready when the input queue is resolvable, with the
// consumer's own access.
func newReadinessCheck(client Consumer, cfg config.Config) health.Check {
	return health.Check{
		Name: "sqs",
		Fn: func(ctx context.Context) error {
			_, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(cfg.SQS.InputQueue)})
			return err
		},
	}
}
