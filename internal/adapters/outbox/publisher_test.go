package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

// --- fakes -------------------------------------------------------------------------

type fakeStore struct {
	mu sync.Mutex

	queue       []application.OutboxEntry // what Reserve hands out, in order
	reserveErr  error
	reserveCall int

	published []uuid.UUID
	failed    []application.OutboxFailure
	released  []uuid.UUID

	publishedErr error
	failedErr    error
	releaseErr   error

	oldest    time.Duration
	hasOldest bool

	lastLimit int
	lastLease time.Duration
}

func (s *fakeStore) Reserve(_ context.Context, limit int, lease time.Duration) ([]application.OutboxEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveCall++
	s.lastLimit, s.lastLease = limit, lease
	if s.reserveErr != nil {
		return nil, s.reserveErr
	}
	n := min(limit, len(s.queue))
	out := s.queue[:n]
	s.queue = s.queue[n:]
	return out, nil
}

func (s *fakeStore) MarkPublished(_ context.Context, ids []uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publishedErr != nil {
		return s.publishedErr
	}
	s.published = append(s.published, ids...)
	return nil
}

func (s *fakeStore) MarkFailed(_ context.Context, failures []application.OutboxFailure) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failedErr != nil {
		return s.failedErr
	}
	s.failed = append(s.failed, failures...)
	return nil
}

func (s *fakeStore) Release(_ context.Context, ids []uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaseErr != nil {
		return s.releaseErr
	}
	s.released = append(s.released, ids...)
	return nil
}

func (s *fakeStore) OldestUnpublished(context.Context) (time.Duration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.oldest, s.hasOldest, nil
}

func (s *fakeStore) snapshot() (published []uuid.UUID, failed []application.OutboxFailure, released []uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.published...), append([]application.OutboxFailure(nil), s.failed...), append([]uuid.UUID(nil), s.released...)
}

type fakeSender struct {
	mu      sync.Mutex
	sent    [][]application.OutboundMessage
	err     error
	refused map[uuid.UUID]error
	block   chan struct{} // when set, Send waits for it (or for the context)
	entered chan struct{} // receives a value each time Send starts
}

func (s *fakeSender) Send(ctx context.Context, messages []application.OutboundMessage) (map[uuid.UUID]error, error) {
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, append([]application.OutboundMessage(nil), messages...))
	if s.err != nil {
		return nil, s.err
	}
	return s.refused, nil
}

func (s *fakeSender) batches() [][]application.OutboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]application.OutboundMessage(nil), s.sent...)
}

var epoch = time.Date(2026, 10, 14, 12, 0, 0, 123_000_000, time.UTC)

func entry(attempts int) application.OutboxEntry {
	id := uuid.New()
	return application.OutboxEntry{
		EventID: id, AggregateID: uuid.New(), EventType: "WalletBalanceChanged", Version: 1,
		Payload:       json.RawMessage(`{"walletId":"w","walletVersion":1}`),
		CorrelationID: "corr-" + id.String()[:4], CausationID: "cause", OccurredAt: epoch, Attempts: attempts,
	}
}

type fixture struct {
	store  *fakeStore
	sender *fakeSender
	p      *Publisher
	logs   *bytes.Buffer
	reg    *prometheus.Registry
}

func newFixture(t *testing.T, entries ...application.OutboxEntry) *fixture {
	t.Helper()
	f := &fixture{store: &fakeStore{queue: entries}, sender: &fakeSender{}, logs: &bytes.Buffer{}, reg: prometheus.NewRegistry()}
	f.p = New(f.store, f.sender, Options{
		PollInterval: 10 * time.Millisecond, BatchSize: 10, Lease: 30 * time.Second,
		Logger: slog.New(slog.NewJSONHandler(&lockedWriter{w: f.logs}, nil)), Registerer: f.reg,
		LagEvery: time.Nanosecond,
		Jitter:   func() float64 { return 0.5 }, // no jitter: the middle of the range
	})
	return f
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func counter(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == name {
			var total float64
			for _, m := range family.GetMetric() {
				total += m.GetCounter().GetValue() + m.GetGauge().GetValue()
			}
			return total
		}
	}
	t.Fatalf("metric %s is not registered", name)
	return 0
}

// --- one cycle ---------------------------------------------------------------------

func TestACycleSendsWhatItReservedAndMarksItPublished(t *testing.T) {
	a, b := entry(0), entry(0)
	f := newFixture(t, a, b)

	handled := f.p.cycle(t.Context())
	require.Equal(t, 2, handled)

	require.Equal(t, 10, f.store.lastLimit)
	require.Equal(t, 30*time.Second, f.store.lastLease)
	batches := f.sender.batches()
	require.Len(t, batches, 1, "one send for the whole batch")
	require.Len(t, batches[0], 2)

	for i, want := range []application.OutboxEntry{a, b} {
		message := batches[0][i]
		require.Equal(t, want.EventID, message.EventID)
		require.Equal(t, want.AggregateID.String(), message.GroupID, "events of a wallet are one message group")
		require.Equal(t, want.EventID.String(), message.DeduplicationID, "a repeated publication is recognised by the event id")
		require.Equal(t, "WalletBalanceChanged", message.EventType)
		require.Equal(t, want.CorrelationID, message.CorrelationID)
		require.JSONEq(t, `{
			"eventId":"`+want.EventID.String()+`","eventType":"WalletBalanceChanged","aggregateId":"`+want.AggregateID.String()+`",
			"correlationId":"`+want.CorrelationID+`","causationId":"cause","occurredAt":"2026-10-14T12:00:00.123Z","version":1,
			"data":{"walletId":"w","walletVersion":1}}`, string(message.Body))
	}

	published, failed, released := f.store.snapshot()
	require.Equal(t, []uuid.UUID{a.EventID, b.EventID}, published)
	require.Empty(t, failed)
	require.Empty(t, released)
	require.Equal(t, 2.0, counter(t, f.reg, "outbox_published_total"))
	require.Zero(t, counter(t, f.reg, "outbox_publish_failures_total"))
	require.Contains(t, f.logs.String(), a.EventID.String(), "the batch log names the events")
}

func TestACycleWithNothingToDoSendsNothing(t *testing.T) {
	f := newFixture(t)
	require.Zero(t, f.p.cycle(t.Context()))
	require.Empty(t, f.sender.batches())
}

func TestAWholeBatchThatFailsIsRetriedWithBackoffByAttempts(t *testing.T) {
	first, third := entry(0), entry(2)
	f := newFixture(t, first, third)
	f.sender.err = errors.New("queue unreachable")

	require.Equal(t, 2, f.p.cycle(t.Context()))

	published, failed, _ := f.store.snapshot()
	require.Empty(t, published)
	require.Len(t, failed, 2)
	require.Equal(t, first.EventID, failed[0].EventID)
	require.Contains(t, failed[0].Reason, "queue unreachable")
	require.Equal(t, time.Second, failed[0].RetryIn, "first failure: the base delay")
	require.Equal(t, 4*time.Second, failed[1].RetryIn, "third failure: the base delay doubled twice")
	require.Equal(t, 2.0, counter(t, f.reg, "outbox_publish_failures_total"))
	require.Equal(t, 1.0, counter(t, f.reg, "outbox_publish_retries_total"), "only the entry that had already failed is a retry")
	require.Zero(t, counter(t, f.reg, "outbox_published_total"))
}

func TestAPartiallyRefusedBatchPublishesTheRestAndRetriesOnlyTheRefused(t *testing.T) {
	ok, refused := entry(0), entry(0)
	f := newFixture(t, ok, refused)
	f.sender.refused = map[uuid.UUID]error{refused.EventID: errors.New("InvalidParameterValue")}

	require.Equal(t, 2, f.p.cycle(t.Context()))
	published, failed, _ := f.store.snapshot()
	require.Equal(t, []uuid.UUID{ok.EventID}, published)
	require.Len(t, failed, 1)
	require.Equal(t, refused.EventID, failed[0].EventID)
	require.Contains(t, failed[0].Reason, "InvalidParameterValue")
}

func TestARowThatCannotBeSerializedFailsAloneAndDoesNotStopTheOthers(t *testing.T) {
	good, corrupt := entry(0), entry(0)
	corrupt.Payload = json.RawMessage(`{not json`)
	f := newFixture(t, good, corrupt)

	f.p.cycle(t.Context())
	batches := f.sender.batches()
	require.Len(t, batches, 1)
	require.Len(t, batches[0], 1, "only the good one is sent")
	published, failed, _ := f.store.snapshot()
	require.Equal(t, []uuid.UUID{good.EventID}, published)
	require.Len(t, failed, 1)
	require.Equal(t, corrupt.EventID, failed[0].EventID)
	require.Contains(t, f.logs.String(), `"level":"ERROR"`)
}

func TestIfMarkingFailsAfterASuccessfulSendTheEventsAreLeftToTheLease(t *testing.T) {
	a := entry(0)
	f := newFixture(t, a)
	f.store.publishedErr = errors.New("database down")

	f.p.cycle(t.Context())
	require.Len(t, f.sender.batches(), 1, "it was sent")
	published, failed, released := f.store.snapshot()
	require.Empty(t, published)
	require.Empty(t, failed, "a failed bookkeeping is not a failed publication: no attempt is counted")
	require.Empty(t, released, "the lease stays, and runs out; another instance sends it again")
	require.Contains(t, f.logs.String(), "database down")
	require.Zero(t, counter(t, f.reg, "outbox_published_total"), "not counted as published until it is recorded")
}

func TestAReserveErrorIsLoggedAndTheCycleMovesOn(t *testing.T) {
	f := newFixture(t)
	f.store.reserveErr = errors.New("connection refused")
	require.Zero(t, f.p.cycle(t.Context()))
	require.Contains(t, f.logs.String(), "connection refused")
	require.Empty(t, f.sender.batches())
}

func TestTheLagGaugeFollowsTheOldestUnpublishedEvent(t *testing.T) {
	f := newFixture(t)
	f.store.oldest, f.store.hasOldest = 42*time.Second, true
	f.p.cycle(t.Context())
	require.InDelta(t, 42, counter(t, f.reg, "outbox_lag_seconds"), 0.001)

	f.store.hasOldest = false
	f.p.cycle(t.Context())
	require.Zero(t, counter(t, f.reg, "outbox_lag_seconds"), "nothing waiting: no lag")
}

// --- backoff -----------------------------------------------------------------------

func TestBackoffGrowsExponentiallyAndIsCapped(t *testing.T) {
	middle := func() float64 { return 0.5 }
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 300 * time.Second}
	for i, w := range want {
		require.Equal(t, w, backoff(i+1, middle), "attempt %d", i+1)
	}
	for _, attempt := range []int{11, 50, 1000, 1 << 30, math.MaxInt32} {
		require.Equal(t, maxBackoff, backoff(attempt, middle), "attempt %d must not overflow", attempt)
	}
}

func TestBackoffJitterStaysWithinTwentyPercent(t *testing.T) {
	low, high := backoff(4, func() float64 { return 0 }), backoff(4, func() float64 { return 1 })
	require.InDelta(t, float64(6400*time.Millisecond), float64(low), float64(time.Microsecond))
	require.InDelta(t, float64(9600*time.Millisecond), float64(high), float64(time.Microsecond))
	require.Less(t, low, high)
}

func TestBackoffNeverReturnsLessThanTheBaseForSillyInput(t *testing.T) {
	require.Equal(t, time.Second, backoff(0, func() float64 { return 0.5 }))
	require.Equal(t, time.Second, backoff(-5, func() float64 { return 0.5 }))
}

// --- the loop ----------------------------------------------------------------------

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	require.Eventually(t, condition, 5*time.Second, 5*time.Millisecond, what)
}

func TestTheLoopPublishesEventsThatAppearLater(t *testing.T) {
	f := newFixture(t)
	f.p.Start()
	t.Cleanup(func() { _ = f.p.Stop(context.Background()) })

	late := entry(0)
	f.store.mu.Lock()
	f.store.queue = append(f.store.queue, late)
	f.store.mu.Unlock()

	waitFor(t, "the late event to be published", func() bool {
		published, _, _ := f.store.snapshot()
		return len(published) == 1
	})
}

func TestTheLoopDoesNotWaitBetweenFullBatches(t *testing.T) {
	var entries []application.OutboxEntry
	for i := 0; i < 30; i++ {
		entries = append(entries, entry(0))
	}
	f := newFixture(t, entries...)
	f.p = New(f.store, f.sender, Options{PollInterval: time.Hour, BatchSize: 10, Lease: time.Minute,
		Logger: slog.New(slog.NewJSONHandler(&lockedWriter{w: f.logs}, nil)), Registerer: prometheus.NewRegistry()})
	f.p.Start()
	t.Cleanup(func() { _ = f.p.Stop(context.Background()) })

	waitFor(t, "three full batches with a one hour poll interval", func() bool {
		published, _, _ := f.store.snapshot()
		return len(published) == 30
	})
}

func TestStoppingWaitsForTheBatchInFlight(t *testing.T) {
	a := entry(0)
	f := newFixture(t, a)
	f.sender.block = make(chan struct{})
	f.sender.entered = make(chan struct{}, 4)
	f.p.Start()

	<-f.sender.entered // the batch is being sent
	stopped := make(chan error, 1)
	go func() { stopped <- f.p.Stop(t.Context()) }()

	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) while a batch was still being sent", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(f.sender.block) // the send completes

	require.NoError(t, <-stopped)
	published, failed, released := f.store.snapshot()
	require.Equal(t, []uuid.UUID{a.EventID}, published, "the batch in flight was finished and recorded")
	require.Empty(t, failed)
	require.Empty(t, released)
}

func TestStoppingAtTheDeadlineReleasesTheBatchInFlight(t *testing.T) {
	a, b := entry(0), entry(0)
	f := newFixture(t, a, b)
	f.sender.block = make(chan struct{}) // never released
	f.sender.entered = make(chan struct{}, 4)
	f.p.Start()
	<-f.sender.entered

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := f.p.Stop(ctx)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 5*time.Second)

	published, failed, released := f.store.snapshot()
	require.Empty(t, published)
	require.Empty(t, failed, "an interrupted send is not a failed attempt")
	require.ElementsMatch(t, []uuid.UUID{a.EventID, b.EventID}, released, "the leases are given back so another instance can take over at once")
}

func TestStoppingTwiceAndStoppingWithoutStartingAreHarmless(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.p.Stop(t.Context()))
	f.p.Start()
	require.NoError(t, f.p.Stop(t.Context()))
	require.NoError(t, f.p.Stop(t.Context()))
}

func TestAPublisherStoppedBeforeItStartedNeverStarts(t *testing.T) {
	f := newFixture(t, entry(0))
	require.NoError(t, f.p.Stop(t.Context()))
	f.p.Start()
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	require.Zero(t, f.store.reserveCall, "a stopped publisher does not come back to life")
}

func TestAStoppedPublisherHasEndedItsLoop(t *testing.T) {
	f := newFixture(t)
	f.p.Start()
	require.NoError(t, f.p.Stop(t.Context()))
	select {
	case <-f.p.done:
	default:
		t.Fatal("Stop returned while the loop was still running: it could reserve more events")
	}
}
