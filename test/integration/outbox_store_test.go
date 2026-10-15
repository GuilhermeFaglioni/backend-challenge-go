//go:build integration

package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

type outboxEnv struct {
	owner *schema
	pool  *pgxpool.Pool
	store *postgres.OutboxStore
}

func newOutboxEnv(t *testing.T) outboxEnv {
	t.Helper()
	s := newSchema(t)
	pool := appPool(s.ctx, t, s.appDSN)
	return outboxEnv{owner: s, pool: pool, store: postgres.NewOutboxStore(pool)}
}

// add writes an event of the wallet and returns its id.
func (e outboxEnv) add(t *testing.T, wallet uuid.UUID) uuid.UUID {
	t.Helper()
	return e.addAt(t, wallet, "", time.Now().UTC())
}

// addAt writes an event with a causation id and an occurrence time. Both are part of
// the immutable content, so they can only be given when the event is written.
func (e outboxEnv) addAt(t *testing.T, wallet uuid.UUID, causation string, occurredAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var cause any
	if causation != "" {
		cause = causation
	}
	require.Empty(t, e.owner.exec(e.owner.owner, `INSERT INTO outbox (event_id, aggregate_id, event_type, version, payload, correlation_id, causation_id, occurred_at)
		VALUES ($1, $2, 'WalletBalanceChanged', 1, $3::jsonb, 'corr', $4, $5)`, id, wallet, `{"id":"`+id.String()+`"}`, cause, occurredAt))
	return id
}

func (e outboxEnv) set(t *testing.T, id uuid.UUID, assignments string) {
	t.Helper()
	require.Empty(t, e.owner.exec(e.owner.owner, `UPDATE outbox SET `+assignments+` WHERE event_id = $1`, id))
}

func ids(entries []application.OutboxEntry) []uuid.UUID {
	out := make([]uuid.UUID, len(entries))
	for i, entry := range entries {
		out[i] = entry.EventID
	}
	return out
}

func (e outboxEnv) row(t *testing.T, id uuid.UUID) (attempts int, published bool, leased bool, lastError *string, retryIn time.Duration) {
	t.Helper()
	var publishedAt, lockedUntil *time.Time
	var nextAttempt, dbNow time.Time
	require.NoError(t, e.owner.owner.QueryRow(e.owner.ctx,
		`SELECT attempts, published_at, locked_until, last_error, next_attempt_at, now() FROM outbox WHERE event_id = $1`, id).
		Scan(&attempts, &publishedAt, &lockedUntil, &lastError, &nextAttempt, &dbNow))
	return attempts, publishedAt != nil, lockedUntil != nil && lockedUntil.After(dbNow), lastError, nextAttempt.Sub(dbNow)
}

func TestReservingReturnsDueEventsInTheOrderTheyWereWrittenAndLeasesThem(t *testing.T) {
	e := newOutboxEnv(t)
	a, b, c := e.add(t, uuid.New()), e.add(t, uuid.New()), e.add(t, uuid.New())

	got, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{a, b, c}, ids(got))
	for _, id := range []uuid.UUID{a, b, c} {
		_, published, leased, _, _ := e.row(t, id)
		require.False(t, published)
		require.True(t, leased, "a reserved event is leased")
	}

	again, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.Empty(t, again, "a leased event is not handed out twice")
}

func TestReservingReturnsTheEventAsStored(t *testing.T) {
	e := newOutboxEnv(t)
	wallet := uuid.New()
	id := e.addAt(t, wallet, "cause-1", time.Now().UTC())

	got, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	entry := got[0]
	require.Equal(t, id, entry.EventID)
	require.Equal(t, wallet, entry.AggregateID)
	require.Equal(t, "WalletBalanceChanged", entry.EventType)
	require.Equal(t, 1, entry.Version)
	require.JSONEq(t, `{"id":"`+id.String()+`"}`, string(entry.Payload))
	require.Equal(t, "corr", entry.CorrelationID)
	require.Equal(t, "cause-1", entry.CausationID)
	require.Zero(t, entry.Attempts)
	require.WithinDuration(t, time.Now(), entry.OccurredAt, time.Minute)
}

func TestReservingRespectsTheLimit(t *testing.T) {
	e := newOutboxEnv(t)
	for i := 0; i < 5; i++ {
		e.add(t, uuid.New())
	}
	got, err := e.store.Reserve(t.Context(), 3, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 3)
}

func TestReservingSkipsWhatIsNotDue(t *testing.T) {
	e := newOutboxEnv(t)
	published, scheduled, leased, expired, due := e.add(t, uuid.New()), e.add(t, uuid.New()), e.add(t, uuid.New()), e.add(t, uuid.New()), e.add(t, uuid.New())
	e.set(t, published, `published_at = now()`)
	e.set(t, scheduled, `next_attempt_at = now() + interval '1 hour'`)
	e.set(t, leased, `locked_until = now() + interval '1 hour'`)
	e.set(t, expired, `locked_until = now() - interval '1 second'`)

	got, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.ElementsMatch(t, []uuid.UUID{expired, due}, ids(got), "a lease that ran out frees the event")
}

// A wallet's events leave in the order they were written: the second waits for the
// first, however the first ends up (published, backing off, or held by someone else).
func TestAWalletsEventsAreReservedOneAtATimeInOrder(t *testing.T) {
	e := newOutboxEnv(t)
	wallet, other := uuid.New(), uuid.New()
	first, second, third := e.add(t, wallet), e.add(t, wallet), e.add(t, wallet)
	elsewhere := e.add(t, other)

	got, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{first, elsewhere}, ids(got), "one event per wallet, and other wallets are not held back")

	t.Run("still blocked while the first is in flight or backing off", func(t *testing.T) {
		again, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
		require.NoError(t, err)
		require.Empty(t, again)

		require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: first, Reason: "queue down", RetryIn: time.Hour}}))
		again, err = e.store.Reserve(t.Context(), 10, 1, time.Minute)
		require.NoError(t, err)
		require.Empty(t, again, "the second event does not overtake the first while it waits to retry")
	})

	t.Run("released when the first is published", func(t *testing.T) {
		e.set(t, first, `next_attempt_at = now()`)
		got, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{first}, ids(got))
		require.NoError(t, e.store.MarkPublished(t.Context(), []uuid.UUID{first, elsewhere}))

		got, err = e.store.Reserve(t.Context(), 10, 1, time.Minute)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{second}, ids(got))
		require.NoError(t, e.store.MarkPublished(t.Context(), []uuid.UUID{second}))
		got, err = e.store.Reserve(t.Context(), 10, 1, time.Minute)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{third}, ids(got))
	})
}

// A publisher must not queue up behind another one: rows that another transaction is
// holding are skipped, and the rest are taken at once.
func TestReservingDoesNotWaitForRowsAnotherTransactionHolds(t *testing.T) {
	e := newOutboxEnv(t)
	held, free := e.add(t, uuid.New()), e.add(t, uuid.New())

	holder, err := e.owner.owner.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	_, err = holder.Exec(t.Context(), `SELECT 1 FROM outbox WHERE event_id = $1 FOR UPDATE`, held)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	got, err := e.store.Reserve(ctx, 10, 1, time.Minute)
	require.NoError(t, err, "the reservation waited for a row that was not its to take")
	require.Equal(t, []uuid.UUID{free}, ids(got))
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestPublishersContendingForTheSameRowsNeverShareOneAndNeverLoseOne(t *testing.T) {
	e := newOutboxEnv(t)
	const events = 200
	want := map[uuid.UUID]bool{}
	for i := 0; i < events; i++ {
		want[e.add(t, uuid.New())] = true
	}

	var mu sync.Mutex
	taken := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for publisher := 0; publisher < 6; publisher++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := postgres.NewOutboxStore(e.pool)
			for {
				got, err := store.Reserve(t.Context(), 7, 1, time.Minute)
				if err != nil {
					t.Errorf("reserve: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, entry := range got {
					taken[entry.EventID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.Len(t, taken, events, "no event was lost")
	for id, n := range taken {
		require.Equal(t, 1, n, "event %s was handed to more than one publisher", id)
		require.True(t, want[id])
	}
}

func TestFailingAnEventCountsTheAttemptAndSchedulesTheNext(t *testing.T) {
	e := newOutboxEnv(t)
	id := e.add(t, uuid.New())
	_, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)

	require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: id, Reason: "queue down", RetryIn: 30 * time.Second}}))
	attempts, published, leased, lastError, retryIn := e.row(t, id)
	require.Equal(t, 1, attempts)
	require.False(t, published)
	require.False(t, leased, "the lease is given back")
	require.NotNil(t, lastError)
	require.Equal(t, "queue down", *lastError)
	require.InDelta(t, 30, retryIn.Seconds(), 5, "the next attempt is scheduled by the database clock")

	got, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.Empty(t, got, "not due yet")

	e.set(t, id, `next_attempt_at = now()`)
	got, err = e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, 1, got[0].Attempts, "the retry knows it is one")

	require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: id, Reason: "again", RetryIn: time.Minute}}))
	attempts, _, _, _, _ = e.row(t, id)
	require.Equal(t, 2, attempts)
}

func TestAVeryLongReasonIsTruncated(t *testing.T) {
	e := newOutboxEnv(t)
	id := e.add(t, uuid.New())
	require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: id, Reason: strings.Repeat("x", 5000), RetryIn: time.Second}}))
	_, _, _, lastError, _ := e.row(t, id)
	require.NotNil(t, lastError)
	require.Len(t, *lastError, 500)
}

// An error text from the network can hold bytes PostgreSQL cannot store; that must not
// turn a failed publication into a failed bookkeeping.
func TestAReasonThatIsNotValidTextIsStillRecorded(t *testing.T) {
	e := newOutboxEnv(t)
	id := e.add(t, uuid.New())
	require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: id, Reason: "bad\x00byte \xff\xfe end", RetryIn: time.Second}}))
	attempts, _, _, lastError, _ := e.row(t, id)
	require.Equal(t, 1, attempts)
	require.NotNil(t, lastError)
	require.Contains(t, *lastError, "end")
	require.NotContains(t, *lastError, "\x00")
}

// Truncating in the middle of a multi-byte character must not leave invalid text.
func TestTruncationNeverCutsACharacterInHalf(t *testing.T) {
	e := newOutboxEnv(t)
	id := e.add(t, uuid.New())
	reason := strings.Repeat("\u00e9", 400) // 800 bytes of two-byte characters
	require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: id, Reason: reason, RetryIn: time.Second}}))
	_, _, _, lastError, _ := e.row(t, id)
	require.NotNil(t, lastError)
	require.True(t, utf8.ValidString(*lastError))
	require.LessOrEqual(t, len(*lastError), 500)
}

func TestMarkingPublishedIsFinalAndIdempotent(t *testing.T) {
	e := newOutboxEnv(t)
	a, b := e.add(t, uuid.New()), e.add(t, uuid.New())
	_, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)

	require.NoError(t, e.store.MarkPublished(t.Context(), []uuid.UUID{a, uuid.New()}), "an unknown id is not an error")
	_, published, leased, _, _ := e.row(t, a)
	require.True(t, published)
	require.False(t, leased)
	require.NoError(t, e.store.MarkPublished(t.Context(), []uuid.UUID{a}), "again, with the same result")
	_, published, _, _, _ = e.row(t, b)
	require.False(t, published, "only the ones named")

	// A failure reported late for something already published changes nothing.
	require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: a, Reason: "late", RetryIn: time.Second}}))
	attempts, published, _, _, _ := e.row(t, a)
	require.True(t, published)
	require.Zero(t, attempts)
	require.NoError(t, e.store.MarkPublished(t.Context(), nil))
	require.NoError(t, e.store.MarkFailed(t.Context(), nil))
	require.NoError(t, e.store.Release(t.Context(), nil))
}

func TestReleasingGivesTheLeaseBackWithoutCountingAnAttempt(t *testing.T) {
	e := newOutboxEnv(t)
	id := e.add(t, uuid.New())
	_, err := e.store.Reserve(t.Context(), 10, 1, time.Hour)
	require.NoError(t, err)

	require.NoError(t, e.store.Release(t.Context(), []uuid.UUID{id}))
	attempts, _, leased, _, _ := e.row(t, id)
	require.Zero(t, attempts)
	require.False(t, leased)
	got, err := e.store.Reserve(t.Context(), 10, 1, time.Minute)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{id}, ids(got), "another publisher can take it at once")
}

func TestTheOldestUnpublishedEventSetsTheLag(t *testing.T) {
	e := newOutboxEnv(t)
	age, any, err := e.store.OldestUnpublished(t.Context())
	require.NoError(t, err)
	require.False(t, any)
	require.Zero(t, age)

	old := e.addAt(t, uuid.New(), "", time.Now().UTC().Add(-90*time.Second))
	fresh := e.add(t, uuid.New())
	age, any, err = e.store.OldestUnpublished(t.Context())
	require.NoError(t, err)
	require.True(t, any)
	require.InDelta(t, 90, age.Seconds(), 5)

	require.NoError(t, e.store.MarkPublished(t.Context(), []uuid.UUID{old}))
	age, any, err = e.store.OldestUnpublished(t.Context())
	require.NoError(t, err)
	require.True(t, any)
	require.Less(t, age, 30*time.Second, "now it is the fresh one")
	require.NoError(t, e.store.MarkPublished(t.Context(), []uuid.UUID{fresh}))
	_, any, err = e.store.OldestUnpublished(t.Context())
	require.NoError(t, err)
	require.False(t, any)
}

func TestACancelledContextIsReportedAsTemporary(t *testing.T) {
	e := newOutboxEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.store.Reserve(ctx, 10, 1, time.Minute)
	require.ErrorIs(t, err, application.ErrTemporarilyUnavailable)
	require.ErrorIs(t, err, context.Canceled)
}

// --- a run of a wallet's events --------------------------------------------------------

func TestAWalletIsReservedWithARunOfItsNextEventsInOrder(t *testing.T) {
	e := newOutboxEnv(t)
	w, other := uuid.New(), uuid.New()
	var run []uuid.UUID
	for range 5 {
		run = append(run, e.add(t, w))
	}
	o1 := e.add(t, other)

	got, err := e.store.Reserve(t.Context(), 10, 3, time.Minute)

	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{run[0], run[1], run[2], o1}, ids(got), "the first three of the wallet, then the other wallet, in the order written")
	for _, id := range run[:3] {
		_, _, leased, _, _ := e.row(t, id)
		require.True(t, leased)
	}
	_, _, leased, _, _ := e.row(t, run[3])
	require.False(t, leased, "beyond the depth nothing is leased")

	again, err := e.store.Reserve(t.Context(), 10, 3, time.Minute)
	require.NoError(t, err)
	require.Empty(t, again, "while its head is held, no publisher takes the rest of the wallet")
}

func TestTheNextRunStartsWhereThePublishedOneEnded(t *testing.T) {
	e := newOutboxEnv(t)
	w := uuid.New()
	var run []uuid.UUID
	for range 4 {
		run = append(run, e.add(t, w))
	}
	first, err := e.store.Reserve(t.Context(), 10, 2, time.Minute)
	require.NoError(t, err)
	require.NoError(t, e.store.MarkPublished(t.Context(), ids(first)))

	second, err := e.store.Reserve(t.Context(), 10, 2, time.Minute)
	require.NoError(t, err)
	require.Equal(t, run[2:], ids(second))
}

func TestAFailedEventHoldsBackTheRestOfItsWallet(t *testing.T) {
	e := newOutboxEnv(t)
	w := uuid.New()
	var run []uuid.UUID
	for range 3 {
		run = append(run, e.add(t, w))
	}
	got, err := e.store.Reserve(t.Context(), 10, 3, time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// The publisher sent the first, the second was refused, the third never went.
	require.NoError(t, e.store.MarkPublished(t.Context(), []uuid.UUID{run[0]}))
	require.NoError(t, e.store.MarkFailed(t.Context(), []application.OutboxFailure{{EventID: run[1], Reason: "throttled", RetryIn: time.Hour}}))
	require.NoError(t, e.store.Release(t.Context(), []uuid.UUID{run[2]}))

	again, err := e.store.Reserve(t.Context(), 10, 3, time.Minute)
	require.NoError(t, err)
	require.Empty(t, again, "the third must not go out before the second, which waits for its retry")
}
