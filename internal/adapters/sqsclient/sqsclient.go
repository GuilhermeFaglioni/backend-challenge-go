// Package sqsclient builds the AWS SQS client shared by the consumer and the
// outbox publisher. Locally it points at LocalStack through AWS_ENDPOINT_URL.
package sqsclient

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
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
func New(cfg config.Config) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.AWS.Region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
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
