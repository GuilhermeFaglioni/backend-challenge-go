// Package sqspublisher sends outbox events to the events queue, an SQS FIFO queue.
package sqspublisher

import (
	"cmp"
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"go.uber.org/fx"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqsclient"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// maxBatch is the most entries SQS takes in one SendMessageBatch.
const maxBatch = 10

// Module provides the sender of the configured events queue.
var Module = fx.Module("sqspublisher",
	fx.Provide(fx.Annotate(NewFromConfig, fx.As(new(application.MessageSender)))),
)

// API is the part of the SQS client the sender uses.
type API interface {
	GetQueueUrl(ctx context.Context, in *sqs.GetQueueUrlInput, opts ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, opts ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
}

// Sender implements application.MessageSender over SQS.
//
// Each message carries `MessageGroupId` (the wallet: a group is delivered in order),
// `MessageDeduplicationId` (the event id: a repeated publication of the same event
// is recognised for five minutes by the queue itself) and, as message attributes,
// `eventType` (so consumers can filter without opening the body) and `correlationId`.
type Sender struct {
	api       API
	queueName string

	mu  sync.Mutex
	url string
}

func New(api API, queueName string) *Sender { return &Sender{api: api, queueName: queueName} }

func NewFromConfig(client sqsclient.Publisher, cfg config.Config) *Sender {
	return New(client, cfg.SQS.EventsQueue)
}

var _ application.MessageSender = (*Sender)(nil)

// queueURL resolves the queue once it exists and remembers it. A failure is not
// remembered: the queue may simply not be there yet.
func (s *Sender) queueURL(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.url != "" {
		return s.url, nil
	}
	out, err := s.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(s.queueName)})
	if err != nil {
		return "", fmt.Errorf("resolve the events queue %q: %w", s.queueName, err)
	}
	s.url = aws.ToString(out.QueueUrl)
	return s.url, nil
}

// Send sends the messages in requests of at most ten. The refused map says, by event
// id, which messages the queue did not take; the error is for a batch of which
// nothing was accepted because of a failure of the request itself.
func (s *Sender) Send(ctx context.Context, messages []application.OutboundMessage) (map[uuid.UUID]error, error) {
	if len(messages) == 0 {
		return nil, nil
	}
	url, err := s.queueURL(ctx)
	if err != nil {
		return nil, err
	}

	refused := map[uuid.UUID]error{}
	var firstRequestErr error
	chunks, failedChunks := 0, 0
	for start := 0; start < len(messages); start += maxBatch {
		chunk := messages[start:min(start+maxBatch, len(messages))]
		chunks++
		failed, err := s.sendChunk(ctx, url, chunk)
		if err != nil {
			failedChunks++
			firstRequestErr = cmp.Or(firstRequestErr, err)
			for _, message := range chunk {
				refused[message.EventID] = err
			}
			continue
		}
		for id, reason := range failed {
			refused[id] = reason
		}
	}
	// Nothing was even asked of the queue successfully: the batch as a whole failed.
	// Otherwise the outcome is per message, so an accepted chunk is not lost sight of.
	if failedChunks == chunks {
		return nil, firstRequestErr
	}
	return refused, nil
}

func (s *Sender) sendChunk(ctx context.Context, url string, chunk []application.OutboundMessage) (map[uuid.UUID]error, error) {
	entries := make([]types.SendMessageBatchRequestEntry, len(chunk))
	for i, m := range chunk {
		entries[i] = types.SendMessageBatchRequestEntry{
			Id:                     aws.String(m.EventID.String()),
			MessageBody:            aws.String(string(m.Body)),
			MessageGroupId:         aws.String(m.GroupID),
			MessageDeduplicationId: aws.String(m.DeduplicationID),
			MessageAttributes: map[string]types.MessageAttributeValue{
				"eventType":     {DataType: aws.String("String"), StringValue: aws.String(m.EventType)},
				"correlationId": {DataType: aws.String("String"), StringValue: aws.String(m.CorrelationID)},
			},
		}
	}
	out, err := s.api.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(url), Entries: entries})
	if err != nil {
		return nil, fmt.Errorf("send to the events queue: %w", err)
	}
	var failed map[uuid.UUID]error
	for _, f := range out.Failed {
		id, parseErr := uuid.Parse(aws.ToString(f.Id))
		if parseErr != nil {
			continue
		}
		if failed == nil {
			failed = map[uuid.UUID]error{}
		}
		failed[id] = fmt.Errorf("the events queue refused the message: %s: %s", aws.ToString(f.Code), aws.ToString(f.Message))
	}
	return failed, nil
}
