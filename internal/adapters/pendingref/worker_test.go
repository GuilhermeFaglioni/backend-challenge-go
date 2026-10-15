package pendingref_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/pendingref"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/resolvepending"
)

type fakeStore struct {
	mu       sync.Mutex
	batches  [][]application.PendingItem
	reserved []struct {
		limit int
		lease time.Duration
	}
	released []uuid.UUID
	stats    application.PendingStats
	err      error

	errors map[uuid.UUID]int // RecordError counts
	holds  []struct{ base, ceiling time.Duration }
	failed []uuid.UUID
	gone   bool // RecordError finds the operation decided
}

func (s *fakeStore) RecordError(_ context.Context, id uuid.UUID, _ string, base, ceiling time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return 0, nil
	}
	if s.errors == nil {
		s.errors = map[uuid.UUID]int{}
	}
	s.errors[id]++
	s.holds = append(s.holds, struct{ base, ceiling time.Duration }{base, ceiling})
	return s.errors[id], nil
}

func (s *fakeStore) Fail(_ context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, id)
	return true, nil
}

func (s *fakeStore) failedIDs() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.failed...)
}

func (s *fakeStore) errorCount(id uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errors[id]
}

func (s *fakeStore) Reserve(_ context.Context, limit int, lease time.Duration) ([]application.PendingItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved = append(s.reserved, struct {
		limit int
		lease time.Duration
	}{limit, lease})
	if s.err != nil {
		return nil, s.err
	}
	if len(s.batches) == 0 {
		return nil, nil
	}
	batch := s.batches[0]
	s.batches = s.batches[1:]
	return batch, nil
}

func (s *fakeStore) Release(_ context.Context, ids []uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, ids...)
	return nil
}

func (s *fakeStore) Stats(context.Context) (application.PendingStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats, nil
}

func (s *fakeStore) releasedIDs() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uuid.UUID(nil), s.released...)
}

type fakeResolver struct {
	mu    sync.Mutex
	tried []uuid.UUID
	fn    func(ctx context.Context, item application.PendingItem) error
}

func (r *fakeResolver) Execute(ctx context.Context, item application.PendingItem) (resolvepending.Result, error) {
	r.mu.Lock()
	r.tried = append(r.tried, item.TransactionID)
	r.mu.Unlock()
	if r.fn != nil {
		if err := r.fn(ctx, item); err != nil {
			return resolvepending.Result{}, err
		}
	}
	return resolvepending.Result{Outcome: resolvepending.OutcomeProcessed, TransactionID: item.TransactionID}, nil
}

func (r *fakeResolver) triedIDs() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.tried...)
}

type observer struct {
	mu    sync.Mutex
	stats []application.PendingStats
}

func (o *observer) Observe(s application.PendingStats) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stats = append(o.stats, s)
}

func items(n int) []application.PendingItem {
	out := make([]application.PendingItem, n)
	for i := range out {
		out[i] = application.PendingItem{TransactionID: uuid.New(), WalletID: uuid.New()}
	}
	return out
}

func ids(in []application.PendingItem) []uuid.UUID {
	out := make([]uuid.UUID, len(in))
	for i, item := range in {
		out[i] = item.TransactionID
	}
	return out
}

func options() pendingref.Options {
	return pendingref.Options{PollInterval: 5 * time.Millisecond, BatchSize: 3, Lease: 7 * time.Second, StatsEvery: 5 * time.Millisecond}
}

func TestItReservesABatchUnderALeaseAndTriesEachOperationInOrder(t *testing.T) {
	batch := items(3)
	store := &fakeStore{batches: [][]application.PendingItem{batch}}
	resolver := &fakeResolver{}
	w := pendingref.New(store, resolver, options())
	w.Start()

	require.Eventually(t, func() bool { return len(resolver.triedIDs()) == 3 }, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))

	require.Equal(t, ids(batch), resolver.triedIDs())
	require.Equal(t, 3, store.reserved[0].limit)
	require.Equal(t, 7*time.Second, store.reserved[0].lease)
	require.Empty(t, store.releasedIDs(), "everything was tried, so there is nothing to give back")
}

func TestItKeepsPollingWhenThereIsNothingToDo(t *testing.T) {
	store := &fakeStore{}
	w := pendingref.New(store, &fakeResolver{}, options())
	w.Start()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.reserved) >= 3
	}, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))
}

func TestAFullBatchIsFollowedByAnotherWithoutWaiting(t *testing.T) {
	store := &fakeStore{batches: [][]application.PendingItem{items(3), items(3), items(2)}}
	resolver := &fakeResolver{}
	opt := options()
	opt.PollInterval = time.Hour // only a full batch can explain the quick second and third
	w := pendingref.New(store, resolver, opt)
	w.Start()
	require.Eventually(t, func() bool { return len(resolver.triedIDs()) == 8 }, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))
}

func TestAFailedTryDoesNotStopTheOthersAndKeepsItsLease(t *testing.T) {
	batch := items(3)
	store := &fakeStore{batches: [][]application.PendingItem{batch}}
	resolver := &fakeResolver{fn: func(_ context.Context, item application.PendingItem) error {
		if item.TransactionID == batch[1].TransactionID {
			return errors.New("database down")
		}
		return nil
	}}
	w := pendingref.New(store, resolver, options())
	w.Start()

	require.Eventually(t, func() bool { return len(resolver.triedIDs()) == 3 }, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))

	require.Equal(t, ids(batch), resolver.triedIDs(), "the one after the failure was tried")
	require.Equal(t, []uuid.UUID{batch[1].TransactionID}, store.releasedIDs(),
		"the failed one is handed back at stop (it was never done); during the run its lease is what keeps it away")
}

func TestAReservationThatFailsIsTriedAgainLater(t *testing.T) {
	store := &fakeStore{err: errors.New("down")}
	w := pendingref.New(store, &fakeResolver{}, options())
	w.Start()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.reserved) >= 2
	}, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))
}

func TestStoppingLetsTheTryInHandFinishAndGivesBackWhatWasNotStarted(t *testing.T) {
	batch := items(3)
	store := &fakeStore{batches: [][]application.PendingItem{batch}}
	entered, proceed := make(chan struct{}), make(chan struct{})
	resolver := &fakeResolver{fn: func(_ context.Context, item application.PendingItem) error {
		if item.TransactionID == batch[0].TransactionID {
			close(entered)
			<-proceed
		}
		return nil
	}}
	w := pendingref.New(store, resolver, options())
	w.Start()
	<-entered

	stopped := make(chan error, 1)
	go func() { stopped <- w.Stop(t.Context()) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a try was in hand")
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	require.NoError(t, <-stopped)

	require.Equal(t, []uuid.UUID{batch[0].TransactionID}, resolver.triedIDs(), "the one in hand was finished, no other was started")
	require.ElementsMatch(t, ids(batch[1:]), store.releasedIDs(), "and the two left over are handed back for another instance")
}

func TestPastTheDeadlineTheTryInHandIsInterruptedAndEverythingIsGivenBack(t *testing.T) {
	batch := items(2)
	store := &fakeStore{batches: [][]application.PendingItem{batch}}
	entered := make(chan struct{})
	resolver := &fakeResolver{fn: func(ctx context.Context, _ application.PendingItem) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	w := pendingref.New(store, resolver, options())
	w.Start()
	<-entered

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.NoError(t, w.Stop(ctx))

	require.ElementsMatch(t, ids(batch), store.releasedIDs())
}

func TestTheGaugesAreRefreshedFromTheDatabase(t *testing.T) {
	store := &fakeStore{stats: application.PendingStats{Waiting: 4, Oldest: 90 * time.Second}}
	obs := &observer{}
	opt := options()
	opt.Observer = obs
	w := pendingref.New(store, &fakeResolver{}, opt)
	w.Start()
	require.Eventually(t, func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		return len(obs.stats) >= 2
	}, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))
	require.Equal(t, application.PendingStats{Waiting: 4, Oldest: 90 * time.Second}, obs.stats[0])
}

func TestStartingAndStoppingAreIdempotentAndAWorkerNeverStartedStopsFine(t *testing.T) {
	never := pendingref.New(&fakeStore{}, &fakeResolver{}, options())
	require.NoError(t, never.Stop(t.Context()))
	never.Start() // after Stop it does nothing
	time.Sleep(20 * time.Millisecond)

	w := pendingref.New(&fakeStore{}, &fakeResolver{}, options())
	w.Start()
	w.Start()
	require.NoError(t, w.Stop(t.Context()))
	require.NoError(t, w.Stop(t.Context()))
}

type outcomes struct {
	mu   sync.Mutex
	seen []resolvepending.Outcome
}

func (o *outcomes) Resolved(x resolvepending.Outcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, x)
}

func (o *outcomes) list() []resolvepending.Outcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]resolvepending.Outcome(nil), o.seen...)
}

// failingWorker has the worker take the same operation in several cycles, one per batch,
// every try failing with cause.
func failingWorker(t *testing.T, cause error, tries, maxErrors int) (*fakeStore, *outcomes, application.PendingItem) {
	t.Helper()
	item := items(1)[0]
	var batches [][]application.PendingItem
	for range tries {
		batches = append(batches, []application.PendingItem{item})
	}
	store := &fakeStore{batches: batches}
	rec := &outcomes{}
	opt := options()
	opt.MaxErrors, opt.Recorder = maxErrors, rec
	opt.ErrorBackoffBase, opt.ErrorBackoffMax = 2*time.Second, time.Minute
	resolver := &fakeResolver{fn: func(context.Context, application.PendingItem) error { return cause }}
	w := pendingref.New(store, resolver, opt)
	w.Start()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.batches) == 0
	}, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return len(resolver.triedIDs()) == tries }, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))
	return store, rec, item
}

func TestAnOutageIsNotCountedAgainstTheOperation(t *testing.T) {
	cause := fmt.Errorf("%w: connection refused", application.ErrTemporarilyUnavailable)
	store, rec, item := failingWorker(t, cause, 6, 3)

	require.Zero(t, store.errorCount(item.TransactionID), "the database being down is not the operation's fault")
	require.Empty(t, store.failedIDs())
	require.Empty(t, rec.list())
}

func TestADefectIsCountedAndAnOperationThatKeepsFailingEndsAsFailed(t *testing.T) {
	store, rec, item := failingWorker(t, errors.New("transaction is not a valid transaction: payload hash mismatch"), 3, 3)

	require.Equal(t, 3, store.errorCount(item.TransactionID))
	require.Equal(t, []uuid.UUID{item.TransactionID}, store.failedIDs(), "failed at the third error, not before")
	require.Equal(t, []resolvepending.Outcome{resolvepending.OutcomeError, resolvepending.OutcomeError, resolvepending.OutcomeError, resolvepending.OutcomeFailed}, rec.list())
	require.Equal(t, 2*time.Second, store.holds[0].base, "held back with the configured backoff")
	require.Equal(t, time.Minute, store.holds[0].ceiling)
}

func TestBelowTheLimitTheOperationIsOnlyHeldBack(t *testing.T) {
	store, rec, item := failingWorker(t, errors.New("a bug"), 2, 3)

	require.Equal(t, 2, store.errorCount(item.TransactionID))
	require.Empty(t, store.failedIDs())
	require.Equal(t, []resolvepending.Outcome{resolvepending.OutcomeError, resolvepending.OutcomeError}, rec.list())
}

func TestWithoutALimitNothingIsCounted(t *testing.T) {
	store, _, item := failingWorker(t, errors.New("a bug"), 3, 0)
	require.Zero(t, store.errorCount(item.TransactionID))
	require.Empty(t, store.failedIDs())
}

func TestAnOperationDecidedMeanwhileIsNotFailed(t *testing.T) {
	item := items(1)[0]
	store := &fakeStore{batches: [][]application.PendingItem{{item}}, gone: true}
	opt := options()
	opt.MaxErrors = 1
	resolver := &fakeResolver{fn: func(context.Context, application.PendingItem) error { return errors.New("a bug") }}
	w := pendingref.New(store, resolver, opt)
	w.Start()
	require.Eventually(t, func() bool { return len(resolver.triedIDs()) == 1 }, 2*time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))
	require.Empty(t, store.failedIDs())
}
