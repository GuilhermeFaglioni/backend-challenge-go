package sqsconsumer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqsconsumer"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
)

// --- a fake SQS ---------------------------------------------------------------------

// fakeSQS plays the queue: it hands out the scripted batches one receive at a time and
// then waits, like a long poll on an empty queue, and it records what the consumer did.
type fakeSQS struct {
	mu       sync.Mutex
	batches  [][]types.Message
	events   []string // "execute msg-1", "delete r-1", "visibility r-1=4", "dlq sqs-1" ...
	sent     []sqs.SendMessageInput
	received chan struct{} // a receive was answered
	failures map[string]error

	urlCalls int
	urlFail  int // the first n GetQueueUrl calls fail
	waiting  int // receives waiting on an empty queue
}

func newFakeSQS(batches ...[]types.Message) *fakeSQS {
	return &fakeSQS{batches: batches, received: make(chan struct{}, 100), failures: map[string]error{}}
}

func (f *fakeSQS) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf(format, args...))
}

func (f *fakeSQS) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeSQS) fail(call string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[call] = err
}

func (f *fakeSQS) failure(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures[call]
}

func (f *fakeSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urlCalls++
	if f.urlCalls <= f.urlFail {
		return nil, errors.New("queue lookup refused")
	}
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("http://sqs/" + aws.ToString(in.QueueName))}, nil
}

func (f *fakeSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	if len(f.batches) > 0 {
		batch := f.batches[0]
		f.batches = f.batches[1:]
		f.mu.Unlock()
		f.received <- struct{}{}
		return &sqs.ReceiveMessageOutput{Messages: batch}, nil
	}
	f.waiting++
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	f.waiting--
	f.mu.Unlock()
	return nil, ctx.Err()
}

// The calls made after the work is done must not depend on a context that the shutdown
// has cancelled, so the fake refuses them when it is.
func (f *fakeSQS) DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.record("delete %s", aws.ToString(in.ReceiptHandle))
	return &sqs.DeleteMessageOutput{}, f.failure("delete")
}

func (f *fakeSQS) ChangeMessageVisibility(ctx context.Context, in *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.record("visibility %s=%d", aws.ToString(in.ReceiptHandle), in.VisibilityTimeout)
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func (f *fakeSQS) SendMessage(ctx context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.failure("send"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.sent = append(f.sent, *in)
	f.mu.Unlock()
	f.record("dlq %s", aws.ToString(in.MessageDeduplicationId))
	return &sqs.SendMessageOutput{}, nil
}

// message builds a delivery of a body, as SQS hands it out.
func message(n int, group string, receives int, bodyText string) types.Message {
	return types.Message{
		MessageId: aws.String(fmt.Sprintf("sqs-%d", n)), ReceiptHandle: aws.String(fmt.Sprintf("r-%d", n)), Body: aws.String(bodyText),
		Attributes: map[string]string{"MessageGroupId": group, "ApproximateReceiveCount": strconv.Itoa(receives)},
	}
}

func valid(id string) string { return body(map[string]string{"messageId": strconv.Quote(id)}) }

// --- a fake use case ----------------------------------------------------------------

type fakeProcessor struct {
	mu     sync.Mutex
	inputs []processwager.Input
	result func(processwager.Input) (processwager.Output, error)
	sqs    *fakeSQS
}

func (p *fakeProcessor) Execute(ctx context.Context, in processwager.Input) (processwager.Output, error) {
	p.mu.Lock()
	p.inputs = append(p.inputs, in)
	p.mu.Unlock()
	p.sqs.record("execute %s", in.Message.ID)
	if p.result == nil {
		return processwager.Output{}, nil
	}
	return p.result(in)
}

func (p *fakeProcessor) executed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for _, in := range p.inputs {
		ids = append(ids, in.Message.ID)
	}
	return ids
}

// --- the harness ---------------------------------------------------------------------

type harness struct {
	q    *fakeSQS
	proc *fakeProcessor
	c    *sqsconsumer.Consumer
	reg  *prometheus.Registry
	logs *bytes.Buffer
}

// prepare builds a consumer without starting it, so a test can arrange the fakes before
// the first message is handled; run starts it.
func prepare(t *testing.T, mutate func(*sqsconsumer.Options), batches ...[]types.Message) *harness {
	t.Helper()
	h := &harness{q: newFakeSQS(batches...), reg: prometheus.NewRegistry(), logs: &bytes.Buffer{}}
	h.proc = &fakeProcessor{sqs: h.q}
	opt := sqsconsumer.Options{
		Name: "wager-transactions.fifo", InputQueue: "wager-transactions.fifo", DeadLetterQueue: "wager-transactions-dlq.fifo",
		Workers: 1, BatchSize: 10, PollWait: time.Second, ProcessingTimeout: 5 * time.Second,
		BackoffBase: 2 * time.Second, BackoffMax: 16 * time.Second, ErrorPause: 10 * time.Millisecond,
		Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Registerer: h.reg,
	}
	if mutate != nil {
		mutate(&opt)
	}
	h.c = sqsconsumer.New(h.q, h.proc, opt)
	t.Cleanup(func() { _ = h.c.Stop(context.Background()) })
	return h
}

func (h *harness) run() *harness {
	h.c.Start()
	return h
}

// settle waits until the consumer has gone through every scripted batch and is waiting
// for more on an empty queue, which is when the log is complete, and then stops it.
func (h *harness) settle(t *testing.T, batches int) {
	t.Helper()
	for range batches {
		select {
		case <-h.q.received:
		case <-time.After(5 * time.Second):
			t.Fatal("the consumer did not receive the scripted batches")
		}
	}
	require.Eventually(t, func() bool {
		h.q.mu.Lock()
		defer h.q.mu.Unlock()
		return len(h.q.batches) == 0 && h.q.waiting > 0
	}, 5*time.Second, time.Millisecond, "the consumer to finish the batches and wait for more")
	require.NoError(t, h.c.Stop(t.Context()))
}

func (h *harness) count(t *testing.T, name, label string) float64 {
	t.Helper()
	families, err := h.reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if label == "" {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
		for _, m := range f.GetMetric() {
			if m.GetLabel()[0].GetValue() == label {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no series %s{%s}", name, label)
	return 0
}

// --- handled -------------------------------------------------------------------------

func TestAHandledMessageIsDeletedAfterTheUseCaseReturns(t *testing.T) {
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1"))})
	h.run().settle(t, 1)

	require.Equal(t, []string{"execute msg-1", "delete r-1"}, h.q.log(), "the delete comes after the commit that Execute stands for")
	in := h.proc.inputs[0]
	require.Equal(t, "msg-1", in.Message.ID)
	require.Equal(t, "wager-transactions.fifo", in.Message.Consumer)
	require.Equal(t, "msg-1", in.CorrelationID, "without a correlation attribute the message id correlates")
	require.Equal(t, "msg-1", in.CausationID)
	require.EqualValues(t, 1, h.count(t, "sqs_messages_received_total", ""))
	require.EqualValues(t, 1, h.count(t, "sqs_messages_handled_total", "handled"))
}

func TestTheCorrelationAttributeOfTheMessageIsUsed(t *testing.T) {
	m := message(1, "w1", 1, valid("msg-1"))
	m.MessageAttributes = map[string]types.MessageAttributeValue{"correlationId": {DataType: aws.String("String"), StringValue: aws.String("corr-77")}}
	h := prepare(t, nil, []types.Message{m})
	h.run().settle(t, 1)
	require.Equal(t, "corr-77", h.proc.inputs[0].CorrelationID)
}

func TestAMessageSeenBeforeIsDeletedAndCountedAsADuplicate(t *testing.T) {
	for name, out := range map[string]processwager.Output{
		"the inbox knew the message":        {MessageAlreadyHandled: true},
		"the idempotency key knew the work": {IdempotentReplay: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1"))})
			h.proc.result = func(processwager.Input) (processwager.Output, error) { return out, nil }
			h.run().settle(t, 1)

			require.Equal(t, []string{"execute msg-1", "delete r-1"}, h.q.log())
			require.EqualValues(t, 1, h.count(t, "sqs_messages_handled_total", "duplicate"))
			require.EqualValues(t, 0, h.count(t, "sqs_messages_handled_total", "handled"))
		})
	}
}

func TestAFailedDeleteIsOnlyAWarning(t *testing.T) {
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1")), message(2, "w2", 1, valid("msg-2"))})
	h.q.fail("delete", errors.New("sqs unavailable"))
	h.run().settle(t, 1)

	require.Equal(t, []string{"execute msg-1", "delete r-1", "execute msg-2", "delete r-2"}, h.q.log(), "the next message goes on")
	require.Contains(t, h.logs.String(), "could not delete a handled message")
}

// --- never handled -------------------------------------------------------------------

func TestAMessageThatCanNeverBeHandledGoesToTheDeadLetterQueueAndStopsBlocking(t *testing.T) {
	cases := map[string]struct {
		body   string
		reason sqsconsumer.Reason
	}{
		"not json":     {`hello`, sqsconsumer.ReasonMalformed},
		"unknown type": {body(map[string]string{"type": `"Other"`}), sqsconsumer.ReasonUnknownType},
		"invalid":      {body(map[string]string{"data": data("walletId", `"nope"`)}), sqsconsumer.ReasonInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := prepare(t, nil, []types.Message{message(1, "w1", 3, c.body), message(2, "w1", 1, valid("msg-2"))})
			h.run().settle(t, 1)

			require.Equal(t, []string{"dlq sqs-1", "delete r-1", "execute msg-2", "delete r-2"}, h.q.log(),
				"copied first, deleted after; the next message of the group is handled")
			sent := h.q.sent[0]
			require.Equal(t, c.body, aws.ToString(sent.MessageBody), "the message travels as it came")
			require.Equal(t, "http://sqs/wager-transactions-dlq.fifo", aws.ToString(sent.QueueUrl))
			require.Equal(t, "w1", aws.ToString(sent.MessageGroupId))
			require.Equal(t, "sqs-1", aws.ToString(sent.MessageDeduplicationId), "a second copy after a crash is not a second entry")
			require.Equal(t, string(c.reason), aws.ToString(sent.MessageAttributes["errorReason"].StringValue))
			require.NotEmpty(t, aws.ToString(sent.MessageAttributes["errorDetail"].StringValue))
			require.Equal(t, "3", aws.ToString(sent.MessageAttributes["receiveCount"].StringValue))
			require.Empty(t, h.proc.executed()[:0])
			require.EqualValues(t, 1, h.count(t, "sqs_dead_letters_total", string(c.reason)))
			require.EqualValues(t, 1, h.count(t, "sqs_messages_handled_total", "dead_lettered"))
		})
	}
}

func TestWhatTheUseCaseRefusesForGoodGoesToTheDeadLetterQueue(t *testing.T) {
	cases := map[string]struct {
		err    error
		reason sqsconsumer.Reason
	}{
		"invalid input":      {fmt.Errorf("%w: %w", processwager.ErrInvalidInput, errors.New("OPENING")), sqsconsumer.ReasonInvalid},
		"key conflict":       {processwager.ErrIdempotencyKeyConflict, sqsconsumer.ReasonKeyConflict},
		"duplicate external": {processwager.ErrDuplicateExternalTransaction, sqsconsumer.ReasonDuplicateExtID},
		"message conflict":   {processwager.ErrMessageContentConflict, sqsconsumer.ReasonMessageConflict},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1"))})
			h.proc.result = func(processwager.Input) (processwager.Output, error) { return processwager.Output{}, c.err }
			h.run().settle(t, 1)

			require.Equal(t, []string{"execute msg-1", "dlq sqs-1", "delete r-1"}, h.q.log())
			require.Equal(t, string(c.reason), aws.ToString(h.q.sent[0].MessageAttributes["errorReason"].StringValue))
			require.Equal(t, "msg-1", aws.ToString(h.q.sent[0].MessageAttributes["messageId"].StringValue))
		})
	}
}

func TestAMessageThatCannotBeCopiedToTheDeadLetterQueueStaysInTheInputQueue(t *testing.T) {
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, `hello`), message(2, "w1", 1, valid("msg-2"))})
	h.q.fail("send", errors.New("dlq unavailable"))
	h.run().settle(t, 1)

	require.Equal(t, []string{"visibility r-1=2", "visibility r-2=0"}, h.q.log(),
		"not deleted, so nothing is lost; held back for a while, and its group does not run ahead of it")
	require.Empty(t, h.proc.executed())
	require.Contains(t, h.logs.String(), "could not send a message to the dead-letter queue")
}

// --- not now -------------------------------------------------------------------------

func TestAMessageThatFailsNowIsKeptAndHeldBackLongerEachTime(t *testing.T) {
	for _, c := range []struct{ receives, seconds int }{{1, 2}, {2, 4}, {3, 8}, {4, 16}, {5, 16}, {50, 16}} {
		t.Run(fmt.Sprintf("received %d times", c.receives), func(t *testing.T) {
			h := prepare(t, nil, []types.Message{message(1, "w1", c.receives, valid("msg-1"))})
			h.proc.result = func(processwager.Input) (processwager.Output, error) {
				return processwager.Output{}, fmt.Errorf("%w: database down", application.ErrTemporarilyUnavailable)
			}
			h.run().settle(t, 1)

			require.Equal(t, []string{"execute msg-1", fmt.Sprintf("visibility r-1=%d", c.seconds)}, h.q.log(), "no delete")
			require.EqualValues(t, 1, h.count(t, "sqs_messages_handled_total", "retried"))
			require.Contains(t, h.logs.String(), fmt.Sprintf(`"visibilitySeconds":%d`, c.seconds))
		})
	}
}

func TestAnUnexpectedErrorIsRetriedNotLost(t *testing.T) {
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1"))})
	h.proc.result = func(processwager.Input) (processwager.Output, error) {
		return processwager.Output{}, errors.New("a bug")
	}
	h.run().settle(t, 1)
	require.Equal(t, []string{"execute msg-1", "visibility r-1=2"}, h.q.log(), "the queue's redrive decides when to give up")
}

func TestATimeoutIsRetried(t *testing.T) {
	h := prepare(t, func(o *sqsconsumer.Options) { o.ProcessingTimeout = 20 * time.Millisecond }, []types.Message{message(1, "w1", 1, valid("msg-1"))})
	h.proc.result = func(in processwager.Input) (processwager.Output, error) {
		time.Sleep(100 * time.Millisecond)
		return processwager.Output{}, context.DeadlineExceeded
	}
	h.run().settle(t, 1)
	require.Equal(t, []string{"execute msg-1", "visibility r-1=2"}, h.q.log())
}

func TestTheRestOfAGroupDoesNotRunAheadOfAMessageThatStays(t *testing.T) {
	h := prepare(t, nil, []types.Message{
		message(1, "w1", 1, valid("msg-1")),
		message(2, "w1", 1, valid("msg-2")),
		message(3, "w2", 1, valid("msg-3")),
		message(4, "w1", 1, valid("msg-4")),
	})
	h.proc.result = func(in processwager.Input) (processwager.Output, error) {
		if in.Message.ID == "msg-1" {
			return processwager.Output{}, fmt.Errorf("%w: down", application.ErrTemporarilyUnavailable)
		}
		return processwager.Output{}, nil
	}
	h.run().settle(t, 1)

	require.Equal(t, []string{
		"execute msg-1", "visibility r-1=2",
		"visibility r-2=0",            // released untouched, to be received after msg-1
		"execute msg-3", "delete r-3", // another group goes on
		"visibility r-4=0",
	}, h.q.log())
	require.Equal(t, []string{"msg-1", "msg-3"}, h.proc.executed())
	require.EqualValues(t, 2, h.count(t, "sqs_messages_handled_total", "released"))
}

// --- shutdown ------------------------------------------------------------------------

func TestStoppingFinishesTheMessageInHandAndReleasesTheRestOfTheBatch(t *testing.T) {
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1")), message(2, "w2", 1, valid("msg-2"))})
	entered, proceed := make(chan struct{}), make(chan struct{})
	h.proc.result = func(in processwager.Input) (processwager.Output, error) {
		close(entered)
		<-proceed
		return processwager.Output{}, nil
	}
	h.run()
	<-h.q.received
	<-entered

	stopped := make(chan error, 1)
	go func() { stopped <- h.c.Stop(t.Context()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) while a message was being handled", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(proceed)
	require.NoError(t, <-stopped)

	require.Equal(t, []string{"execute msg-1", "delete r-1", "visibility r-2=0"}, h.q.log(),
		"the one in hand is finished and deleted, the other is given back unhandled")
	require.Equal(t, []string{"msg-1"}, h.proc.executed())
}

func TestPastTheDeadlineTheMessageInHandIsReleasedNotLost(t *testing.T) {
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1")), message(2, "w1", 1, valid("msg-2"))})
	entered := make(chan struct{})
	h.proc.result = func(in processwager.Input) (processwager.Output, error) {
		close(entered)
		time.Sleep(50 * time.Millisecond)
		return processwager.Output{}, context.Canceled // the handling was cancelled by the shutdown
	}
	h.run()
	<-h.q.received
	<-entered

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := h.c.Stop(ctx)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, []string{"execute msg-1", "visibility r-1=0", "visibility r-2=0"}, h.q.log(), "released, not delayed and not deleted")
}

func TestAMessageCommittedWhileStoppingIsStillDeleted(t *testing.T) {
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, valid("msg-1"))})
	entered, proceed := make(chan struct{}), make(chan struct{})
	h.proc.result = func(in processwager.Input) (processwager.Output, error) {
		close(entered)
		<-proceed
		return processwager.Output{}, nil
	}
	h.run()
	<-h.q.received
	<-entered
	go func() { time.Sleep(30 * time.Millisecond); close(proceed) }()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_ = h.c.Stop(ctx) // the deadline passes while the use case is still committing

	require.Contains(t, h.q.log(), "delete r-1", "the work is committed, so the message must go")
}

func TestStoppingAnIdleConsumerIsImmediateAndRepeatable(t *testing.T) {
	h := prepare(t, func(o *sqsconsumer.Options) { o.Workers = 3 }).run()
	done := make(chan struct{})
	go func() {
		require.NoError(t, h.c.Stop(t.Context()))
		require.NoError(t, h.c.Stop(t.Context()))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return while idle")
	}
}

// --- the queue --------------------------------------------------------------------------

func TestTheQueuesAreFoundAgainWhenTheLookupFails(t *testing.T) {
	q := newFakeSQS([]types.Message{message(1, "w1", 1, valid("msg-1"))})
	q.urlFail = 2
	h := &harness{q: q, reg: prometheus.NewRegistry(), logs: &bytes.Buffer{}}
	h.proc = &fakeProcessor{sqs: q}
	h.c = sqsconsumer.New(q, h.proc, sqsconsumer.Options{
		Name: "n", InputQueue: "in.fifo", DeadLetterQueue: "dlq.fifo", Workers: 1, BatchSize: 10, ProcessingTimeout: time.Second,
		BackoffBase: time.Second, BackoffMax: time.Second, ErrorPause: 5 * time.Millisecond, Logger: slog.New(slog.NewJSONHandler(h.logs, nil)), Registerer: h.reg,
	})
	h.c.Start()
	t.Cleanup(func() { _ = h.c.Stop(context.Background()) })
	h.run().settle(t, 1)

	require.Equal(t, []string{"execute msg-1", "delete r-1"}, q.log())
	require.Contains(t, h.logs.String(), "could not find the queues")
}

func TestEveryWorkerReceivesOnItsOwn(t *testing.T) {
	h := prepare(t, func(o *sqsconsumer.Options) { o.Workers = 4 }).run()
	// With nothing to receive each worker waits in its own receive.
	require.Eventually(t, func() bool {
		h.q.mu.Lock()
		defer h.q.mu.Unlock()
		return h.q.waiting == 4
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, h.c.Stop(t.Context()))
	h.q.mu.Lock()
	defer h.q.mu.Unlock()
	require.Zero(t, h.q.waiting, "and every one of them ends when the consumer stops")
}

// --- what is measured and what is not logged --------------------------------------------

func TestMetricsStartAtZero(t *testing.T) {
	h := prepare(t, nil).run()
	require.NoError(t, h.c.Stop(t.Context()))
	for _, result := range []string{"handled", "duplicate", "retried", "dead_lettered", "released"} {
		require.EqualValues(t, 0, h.count(t, "sqs_messages_handled_total", result))
	}
	for _, reason := range sqsconsumer.Reasons {
		require.EqualValues(t, 0, h.count(t, "sqs_dead_letters_total", string(reason)))
	}
	require.Equal(t, 5+len(sqsconsumer.Reasons)+1, testutil.CollectAndCount(h.reg), "every series exists before the first message")
}

func TestNothingOfTheMessageIsLogged(t *testing.T) {
	secret := body(map[string]string{"data": data("money", `{"amount":"SECRET-AMOUNT","currency":"BRL"}`)})
	failing := valid("msg-2")
	h := prepare(t, nil, []types.Message{message(1, "w1", 1, secret), message(2, "w2", 1, failing)})
	h.proc.result = func(processwager.Input) (processwager.Output, error) {
		return processwager.Output{}, fmt.Errorf("%w: down", application.ErrTemporarilyUnavailable)
	}
	h.run().settle(t, 1)

	logged := h.logs.String()
	require.NotEmpty(t, logged)
	for _, text := range []string{"SECRET-AMOUNT", "provider-a:transaction-123", "25.00", playerID, walletID} {
		require.NotContains(t, logged, text)
	}
}

func TestBackoffDoublesFromTheBaseUpToTheCeiling(t *testing.T) {
	cases := []struct {
		base, ceiling time.Duration
		received      int
		want          time.Duration
	}{
		{2 * time.Second, time.Minute, 1, 2 * time.Second},
		{2 * time.Second, time.Minute, 2, 4 * time.Second},
		{2 * time.Second, time.Minute, 5, 32 * time.Second},
		{2 * time.Second, time.Minute, 6, time.Minute},
		{2 * time.Second, time.Minute, 1000, time.Minute},
		{2 * time.Second, time.Minute, 0, 2 * time.Second},
		{2 * time.Second, time.Minute, -3, 2 * time.Second},
		{1500 * time.Millisecond, time.Minute, 1, time.Second},
		{time.Second, 5 * time.Second, 4, 5 * time.Second},
	}
	for _, c := range cases {
		require.Equal(t, c.want, sqsconsumer.Backoff(c.base, c.ceiling, c.received), "%+v", c)
	}
}
