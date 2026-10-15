package sqspublisher

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

type fakeAPI struct {
	mu sync.Mutex

	urlCalls   int
	urlErr     error
	sendCalls  []*sqs.SendMessageBatchInput
	sendErr    error
	sendErrAt  map[int]error // by call index (0-based)
	failedByID map[string]types.BatchResultErrorEntry
}

func (f *fakeAPI) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urlCalls++
	if f.urlErr != nil {
		return nil, f.urlErr
	}
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("http://queue/" + aws.ToString(in.QueueName))}, nil
}

func (f *fakeAPI) SendMessageBatch(_ context.Context, in *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.sendCalls)
	f.sendCalls = append(f.sendCalls, in)
	if err, ok := f.sendErrAt[index]; ok {
		return nil, err
	}
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	out := &sqs.SendMessageBatchOutput{}
	for _, entry := range in.Entries {
		if failed, bad := f.failedByID[aws.ToString(entry.Id)]; bad {
			out.Failed = append(out.Failed, failed)
			continue
		}
		out.Successful = append(out.Successful, types.SendMessageBatchResultEntry{Id: entry.Id})
	}
	return out, nil
}

func message(body string) application.OutboundMessage {
	id := uuid.New()
	return application.OutboundMessage{
		EventID: id, GroupID: uuid.NewString(), DeduplicationID: id.String(),
		EventType: "WalletBalanceChanged", CorrelationID: "corr-1", Body: []byte(body),
	}
}

func TestEachMessageIsSentAsAFifoEntryWithItsGroupDeduplicationIdAndAttributes(t *testing.T) {
	api := &fakeAPI{}
	sender := New(api, "wager-events.fifo")
	a, b := message(`{"a":1}`), message(`{"b":2}`)

	refused, err := sender.Send(t.Context(), []application.OutboundMessage{a, b})
	require.NoError(t, err)
	require.Empty(t, refused)

	require.Len(t, api.sendCalls, 1, "one request for the batch")
	call := api.sendCalls[0]
	require.Equal(t, "http://queue/wager-events.fifo", aws.ToString(call.QueueUrl))
	require.Len(t, call.Entries, 2)
	for i, want := range []application.OutboundMessage{a, b} {
		entry := call.Entries[i]
		require.Equal(t, want.EventID.String(), aws.ToString(entry.Id))
		require.Equal(t, string(want.Body), aws.ToString(entry.MessageBody))
		require.Equal(t, want.GroupID, aws.ToString(entry.MessageGroupId))
		require.Equal(t, want.EventID.String(), aws.ToString(entry.MessageDeduplicationId), "the event id is the deduplication id")
		require.Equal(t, "WalletBalanceChanged", aws.ToString(entry.MessageAttributes["eventType"].StringValue))
		require.Equal(t, "String", aws.ToString(entry.MessageAttributes["eventType"].DataType))
		require.Equal(t, "corr-1", aws.ToString(entry.MessageAttributes["correlationId"].StringValue))
	}
}

func TestTheQueueUrlIsResolvedOnceAndRemembered(t *testing.T) {
	api := &fakeAPI{}
	sender := New(api, "q.fifo")
	for i := 0; i < 3; i++ {
		_, err := sender.Send(t.Context(), []application.OutboundMessage{message("{}")})
		require.NoError(t, err)
	}
	require.Equal(t, 1, api.urlCalls)
}

func TestAnUnresolvableQueueFailsTheBatchAndIsTriedAgainNextTime(t *testing.T) {
	api := &fakeAPI{urlErr: errors.New("queue does not exist")}
	sender := New(api, "q.fifo")

	_, err := sender.Send(t.Context(), []application.OutboundMessage{message("{}")})
	require.ErrorContains(t, err, "queue does not exist")
	require.Empty(t, api.sendCalls)

	api.urlErr = nil
	refused, err := sender.Send(t.Context(), []application.OutboundMessage{message("{}")})
	require.NoError(t, err, "the failure was not remembered")
	require.Empty(t, refused)
	require.Equal(t, 2, api.urlCalls)
}

func TestTheEntriesTheQueueRefusesAreReportedByEventId(t *testing.T) {
	a, b := message("{}"), message("{}")
	api := &fakeAPI{failedByID: map[string]types.BatchResultErrorEntry{
		b.EventID.String(): {Id: aws.String(b.EventID.String()), Code: aws.String("InvalidParameterValue"), Message: aws.String("bad group"), SenderFault: true},
	}}
	refused, err := New(api, "q.fifo").Send(t.Context(), []application.OutboundMessage{a, b})
	require.NoError(t, err)
	require.Len(t, refused, 1)
	require.ErrorContains(t, refused[b.EventID], "InvalidParameterValue")
	require.ErrorContains(t, refused[b.EventID], "bad group")
	require.NotContains(t, refused, a.EventID)
}

func TestARequestThatFailsWholeIsAnErrorForTheBatch(t *testing.T) {
	api := &fakeAPI{sendErr: errors.New("connection refused")}
	refused, err := New(api, "q.fifo").Send(t.Context(), []application.OutboundMessage{message("{}"), message("{}")})
	require.ErrorContains(t, err, "connection refused")
	require.Empty(t, refused)
}

func TestMoreThanTenMessagesAreSplitIntoRequestsOfTenAndAFailedChunkDoesNotHideAnAcceptedOne(t *testing.T) {
	var messages []application.OutboundMessage
	for i := 0; i < 23; i++ {
		messages = append(messages, message("{}"))
	}
	api := &fakeAPI{sendErrAt: map[int]error{1: errors.New("throttled")}}
	refused, err := New(api, "q.fifo").Send(t.Context(), messages)
	require.NoError(t, err, "some were accepted: the outcome is per message")
	require.Len(t, api.sendCalls, 3)
	require.Len(t, api.sendCalls[0].Entries, 10)
	require.Len(t, api.sendCalls[1].Entries, 10)
	require.Len(t, api.sendCalls[2].Entries, 3)

	require.Len(t, refused, 10, "exactly the second chunk")
	for _, entry := range api.sendCalls[1].Entries {
		id := uuid.MustParse(aws.ToString(entry.Id))
		require.ErrorContains(t, refused[id], "throttled")
	}
}

func TestNothingToSendIsNotARequest(t *testing.T) {
	api := &fakeAPI{}
	refused, err := New(api, "q.fifo").Send(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, refused)
	require.Zero(t, api.urlCalls)
	require.Empty(t, api.sendCalls)
}
