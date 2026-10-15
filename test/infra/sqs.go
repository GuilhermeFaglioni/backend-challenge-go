package infra

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
)

// NewSQSClient connects to LocalStack's SQS.
func NewSQSClient(ctx context.Context, endpoint string) (*sqs.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		return nil, err
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) }), nil
}

// CreateFIFOQueue creates a FIFO queue of the test's own, so that what it receives
// is only what the test caused, and removes it when the test ends. It returns the
// queue name (to configure the application with) and its URL.
func CreateFIFOQueue(ctx context.Context, t testing.TB, client *sqs.Client, prefix string) (name, url string) {
	t.Helper()
	name = fmt.Sprintf("%s-%s.fifo", prefix, uuid.NewString()[:8])
	out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String(name),
		Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false"},
	})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}
	url = aws.ToString(out.QueueUrl)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = client.DeleteQueue(cleanup, &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
	})
	return name, url
}

// QueuedMessage is a message taken from a queue.
type QueuedMessage struct {
	Body            string
	GroupID         string
	DeduplicationID string
	Attributes      map[string]string // message attributes, by name
}

// ReceiveMessages takes messages from the queue, deleting each, until it has want or
// the time is up, and returns what it got in the order it got them.
func ReceiveMessages(ctx context.Context, client *sqs.Client, url string, want int, within time.Duration) ([]QueuedMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	var got []QueuedMessage
	for len(got) < want {
		out, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:                    aws.String(url),
			MaxNumberOfMessages:         10,
			WaitTimeSeconds:             1,
			MessageAttributeNames:       []string{"All"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameAll},
		})
		if err != nil {
			if ctx.Err() != nil {
				return got, nil // out of time: the caller compares with what it wanted
			}
			return got, err
		}
		for _, m := range out.Messages {
			attrs := map[string]string{}
			for name, value := range m.MessageAttributes {
				attrs[name] = aws.ToString(value.StringValue)
			}
			got = append(got, QueuedMessage{
				Body:            aws.ToString(m.Body),
				GroupID:         m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)],
				DeduplicationID: m.Attributes[string(types.MessageSystemAttributeNameMessageDeduplicationId)],
				Attributes:      attrs,
			})
			_, _ = client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: m.ReceiptHandle})
		}
	}
	return got, nil
}
