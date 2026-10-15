// Package sqsconsumer reads the input queue and hands each message to the same use case
// the HTTP endpoint uses (ARCHITECTURE.md, section 12).
//
// A message is deleted from the queue only after the transaction that handled it has
// committed, and what the transaction commits includes the record of the message in the
// inbox. So a message that arrives again, because the process died between the commit
// and the delete, or because the queue delivers at least once, is recognised and changes
// nothing.
//
// What happens to a message depends on why it could not be handled, never on a guess:
//
//   - handled (processed, or refused by a rule and stored, or already seen): deleted;
//   - cannot be handled whatever the circumstances (not JSON, an unknown type, a field
//     the rules refuse, a key or message id reused with other content): copied to the
//     dead-letter queue and deleted, so it stops blocking its message group;
//   - could not be handled now (the database is unavailable, the time ran out, anything
//     unexpected): left in the queue, invisible for longer each time it is received, until
//     it works or the queue's redrive policy sends it to the dead-letter queue.
package sqsconsumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
)

// API is what the consumer uses of SQS; *sqs.Client implements it.
type API interface {
	GetQueueUrl(ctx context.Context, in *sqs.GetQueueUrlInput, opts ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, opts ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// Processor is the use case that handles a request.
type Processor interface {
	Execute(ctx context.Context, in processwager.Input) (processwager.Output, error)
}

// Options configure a Consumer.
type Options struct {
	// Name identifies the consumer in the inbox.
	Name            string
	InputQueue      string
	DeadLetterQueue string

	Workers           int
	BatchSize         int
	PollWait          time.Duration
	ProcessingTimeout time.Duration
	BackoffBase       time.Duration
	BackoffMax        time.Duration
	// ErrorPause is how long a worker waits after SQS itself failed. Default 1s.
	ErrorPause time.Duration

	Logger     *slog.Logger
	Registerer prometheus.Registerer
}

// detachedTimeout bounds the calls made after the work is done (delete, visibility,
// dead letter). They are not tied to the polling context: a message that was committed
// must still be deleted while the consumer is shutting down.
const detachedTimeout = 5 * time.Second

// Consumer reads the input queue.
type Consumer struct {
	api  API
	proc Processor
	opt  Options
	log  *slog.Logger
	m    *metrics

	work       context.Context //nolint:containedctx // the lifetime of the handling of messages
	cancelWork context.CancelFunc
	poll       context.Context //nolint:containedctx // the lifetime of the receive loops
	cancelPoll context.CancelFunc

	mu      sync.Mutex // guards the fields below
	started bool
	urls    *queueURLs
	wg      sync.WaitGroup
}

type queueURLs struct{ input, deadLetter string }

// New builds a consumer. It does nothing until Start.
func New(api API, proc Processor, opt Options) *Consumer {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.ErrorPause <= 0 {
		opt.ErrorPause = time.Second
	}
	c := &Consumer{api: api, proc: proc, opt: opt, log: opt.Logger, m: newMetrics(opt.Registerer)}
	c.work, c.cancelWork = context.WithCancel(context.Background())
	c.poll, c.cancelPoll = context.WithCancel(c.work)
	return c
}

// Start begins receiving. Calling it again does nothing.
func (c *Consumer) Start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return
	}
	c.started = true
	for range c.opt.Workers {
		c.wg.Add(1)
		go c.run()
	}
	c.log.Info("sqs consumer started", slog.String("queue", c.opt.InputQueue), slog.Int("workers", c.opt.Workers))
}

// Stop stops receiving and waits for the messages being handled to finish. If ctx ends
// first, the handling is cancelled and the messages not finished are released to the
// queue, so another instance receives them at once; Stop then returns ctx's error.
func (c *Consumer) Stop(ctx context.Context) error {
	c.cancelPoll()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()

	var err error
	select {
	case <-done:
	case <-ctx.Done():
		c.cancelWork()
		<-done
		err = fmt.Errorf("sqs consumer: the messages in flight did not finish in time and were released: %w", ctx.Err())
	}
	c.cancelWork()
	c.log.Info("sqs consumer stopped")
	return err
}

func (c *Consumer) run() {
	defer c.wg.Done()
	for c.poll.Err() == nil {
		urls, err := c.queues(c.poll)
		if err != nil {
			c.pause(err, "could not find the queues")
			continue
		}
		out, err := c.api.ReceiveMessage(c.poll, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(urls.input),
			MaxNumberOfMessages:   clamp32(int64(c.opt.BatchSize)),
			WaitTimeSeconds:       clamp32(int64(c.opt.PollWait / time.Second)),
			MessageAttributeNames: []string{"correlationId"},
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameApproximateReceiveCount, types.MessageSystemAttributeNameMessageGroupId,
			},
		})
		if err != nil {
			c.pause(err, "could not receive from the queue")
			continue
		}
		c.batch(urls, out.Messages)
	}
}

// pause waits after SQS itself failed, unless the consumer is stopping.
func (c *Consumer) pause(err error, what string) {
	if c.poll.Err() != nil {
		return
	}
	c.log.Warn("sqs consumer: "+what, slog.Any("error", err))
	t := time.NewTimer(c.opt.ErrorPause)
	defer t.Stop()
	select {
	case <-t.C:
	case <-c.poll.Done():
	}
}

// queues finds the URLs of the input and dead-letter queues, once.
func (c *Consumer) queues(ctx context.Context) (queueURLs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.urls != nil {
		return *c.urls, nil
	}
	var urls queueURLs
	for _, q := range []struct {
		name string
		into *string
	}{{c.opt.InputQueue, &urls.input}, {c.opt.DeadLetterQueue, &urls.deadLetter}} {
		out, err := c.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.name)})
		if err != nil {
			return queueURLs{}, fmt.Errorf("queue %s: %w", q.name, err)
		}
		*q.into = aws.ToString(out.QueueUrl)
	}
	c.urls = &urls
	return urls, nil
}

// batch handles the messages of one receive, in the order received. Messages of one
// group are ordered, so once one of them is left in the queue the rest of its group in
// this batch must not run ahead of it: they are released untouched.
func (c *Consumer) batch(urls queueURLs, messages []types.Message) {
	blocked := map[string]bool{}
	for _, m := range messages {
		group := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
		c.m.received.Inc()
		if c.poll.Err() != nil || blocked[group] {
			c.release(urls, m)
			continue
		}
		if !c.handle(urls, m) {
			blocked[group] = true
		}
	}
}

// handle takes one message to its end. It returns false when the message stays in the
// queue, which holds back the rest of its group.
func (c *Consumer) handle(urls queueURLs, m types.Message) bool {
	body := aws.ToString(m.Body)
	decoded, problem := Decode([]byte(body))
	if problem != nil {
		return c.deadLetter(urls, m, "", problem.Reason, problem.Detail)
	}
	correlation := ""
	if attr, ok := m.MessageAttributes["correlationId"]; ok {
		correlation = aws.ToString(attr.StringValue)
	}
	if correlation == "" {
		correlation = decoded.MessageID
	}

	ctx, cancel := context.WithTimeout(c.work, c.opt.ProcessingTimeout)
	out, err := c.proc.Execute(ctx, decoded.Input(c.opt.Name, correlation))
	cancel()

	if err == nil {
		// The handling is committed; the queue still holds the message.
		faultinject.Hit(faultinject.ConsumerAfterCommit)
		if out.MessageAlreadyHandled || out.IdempotentReplay {
			c.m.handled.WithLabelValues(resultDuplicate).Inc()
		} else {
			c.m.handled.WithLabelValues(resultHandled).Inc()
		}
		c.delete(urls, m, decoded.MessageID)
		return true
	}
	if reason, never := permanent(err); never {
		return c.deadLetter(urls, m, decoded.MessageID, reason, err.Error())
	}
	if c.work.Err() != nil {
		c.release(urls, m) // shutting down past the deadline: not this message's fault
		return false
	}
	c.retry(urls, m, decoded.MessageID, correlation, err)
	return false
}

// permanent says whether an error means the message can never be handled.
func permanent(err error) (Reason, bool) {
	switch {
	case errors.Is(err, processwager.ErrInvalidInput):
		return ReasonInvalid, true
	case errors.Is(err, processwager.ErrUnsupportedKind):
		return ReasonUnsupportedKind, true
	case errors.Is(err, processwager.ErrIdempotencyKeyConflict):
		return ReasonKeyConflict, true
	case errors.Is(err, processwager.ErrDuplicateExternalTransaction):
		return ReasonDuplicateExtID, true
	case errors.Is(err, processwager.ErrMessageContentConflict):
		return ReasonMessageConflict, true
	}
	return "", false
}

func detached() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), detachedTimeout)
}

// delete removes a message that has been handled. If it fails the message comes back
// and is recognised, so this is a warning, not a failure.
func (c *Consumer) delete(urls queueURLs, m types.Message, messageID string) {
	ctx, cancel := detached()
	defer cancel()
	_, err := c.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(urls.input), ReceiptHandle: m.ReceiptHandle})
	if err != nil {
		c.log.Warn("could not delete a handled message; it will be delivered again and recognised",
			slog.String("messageId", messageID), slog.String("sqsMessageId", aws.ToString(m.MessageId)), slog.Any("error", err))
	}
}

// deadLetter copies the message to the dead-letter queue, with why, and removes it from
// the input queue. If the copy cannot be made the message stays where it is.
func (c *Consumer) deadLetter(urls queueURLs, m types.Message, messageID string, reason Reason, detail string) bool {
	ctx, cancel := detached()
	defer cancel()
	attrs := map[string]types.MessageAttributeValue{
		"errorReason":  stringAttr(string(reason)),
		"errorDetail":  stringAttr(truncate(detail, 256)),
		"receiveCount": stringAttr(strconv.Itoa(receiveCount(m))),
	}
	if messageID != "" {
		attrs["messageId"] = stringAttr(messageID)
	}
	if v, ok := m.MessageAttributes["correlationId"]; ok && aws.ToString(v.StringValue) != "" {
		attrs["correlationId"] = stringAttr(aws.ToString(v.StringValue))
	}
	_, err := c.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               aws.String(urls.deadLetter),
		MessageBody:            m.Body,
		MessageGroupId:         aws.String(groupOf(m)),
		MessageDeduplicationId: m.MessageId, // a second copy, after a crash before the delete, is not a second entry
		MessageAttributes:      attrs,
	})
	if err != nil {
		c.log.Error("could not send a message to the dead-letter queue; it stays in the input queue",
			slog.String("reason", string(reason)), slog.String("sqsMessageId", aws.ToString(m.MessageId)), slog.Any("error", err))
		c.retry(urls, m, messageID, "", err)
		return false
	}
	c.m.handled.WithLabelValues(resultDeadLettered).Inc()
	c.m.deadLetters.WithLabelValues(string(reason)).Inc()
	c.log.Warn("message sent to the dead-letter queue",
		slog.String("reason", string(reason)), slog.String("messageId", messageID), slog.String("sqsMessageId", aws.ToString(m.MessageId)))
	c.delete(urls, m, messageID)
	return true
}

// retry leaves the message in the queue, invisible for longer each time it has been
// received.
func (c *Consumer) retry(urls queueURLs, m types.Message, messageID, correlation string, cause error) {
	received := receiveCount(m)
	delay := Backoff(c.opt.BackoffBase, c.opt.BackoffMax, received)
	ctx, cancel := detached()
	defer cancel()
	_, err := c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(urls.input), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: clamp32(int64(delay / time.Second)),
	})
	c.m.handled.WithLabelValues(resultRetried).Inc()
	attrs := []any{
		slog.String("messageId", messageID), slog.String("correlationId", correlation),
		slog.Int("receiveCount", received), slog.Int64("visibilitySeconds", int64(delay/time.Second)), slog.Any("error", cause),
	}
	if err != nil {
		attrs = append(attrs, slog.Any("visibilityError", err))
	}
	c.log.Warn("message could not be handled now; it will be received again", attrs...)
}

// release makes a message visible again at once, untouched.
func (c *Consumer) release(urls queueURLs, m types.Message) {
	ctx, cancel := detached()
	defer cancel()
	if _, err := c.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(urls.input), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 0,
	}); err != nil {
		c.log.Warn("could not release a message; it becomes visible when its timeout ends",
			slog.String("sqsMessageId", aws.ToString(m.MessageId)), slog.Any("error", err))
	}
	c.m.handled.WithLabelValues(resultReleased).Inc()
}

// Backoff is how long a message that failed stays invisible when it has been received
// `received` times: base, then twice that, and so on, never more than ceiling, and never
// less than a second (SQS visibility has the resolution of a second).
func Backoff(base, ceiling time.Duration, received int) time.Duration {
	delay := base
	for i := 1; i < received && delay < ceiling; i++ {
		delay *= 2
	}
	return max(min(delay, ceiling).Truncate(time.Second), time.Second)
}

// clamp32 fits a count the configuration has already bounded into the int32 SQS takes.
func clamp32(n int64) int32 {
	return int32(max(0, min(n, math.MaxInt32))) //nolint:gosec // bounded just above
}

func receiveCount(m types.Message) int {
	n, err := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func groupOf(m types.Message) string {
	if g := m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]; g != "" {
		return g
	}
	return "unknown"
}

func stringAttr(s string) types.MessageAttributeValue {
	return types.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(s)}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
