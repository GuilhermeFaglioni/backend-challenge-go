//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
)

type retentionFixture struct {
	s     *schema
	store *postgres.RetentionStore
}

func newRetentionFixture(t *testing.T) retentionFixture {
	t.Helper()
	s := newSchema(t)
	return retentionFixture{s: s, store: postgres.NewRetentionStore(appPool(s.ctx, t, s.appDSN))}
}

// message records a handled message completed `age` ago (or a message still in hand).
func (f retentionFixture) message(t *testing.T, id string, age time.Duration, completed bool) {
	t.Helper()
	completedAt := "NULL"
	if completed {
		completedAt = fmt.Sprintf("now() - make_interval(secs => %f)", age.Seconds())
	}
	require.Empty(t, f.s.exec(f.s.owner, `INSERT INTO inbox (consumer_name, message_id, payload_hash, received_at, completed_at)
		VALUES ('c', $1, $2, now() - make_interval(secs => $3), `+completedAt+`)`, id, hash64, age.Seconds()+1))
}

// event writes an outbox event published `age` ago, or not yet published.
func (f retentionFixture) event(t *testing.T, age time.Duration, published bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	publishedAt := "NULL"
	if published {
		publishedAt = fmt.Sprintf("now() - make_interval(secs => %f)", age.Seconds())
	}
	require.Empty(t, f.s.exec(f.s.owner, `INSERT INTO outbox (event_id, aggregate_id, event_type, version, payload, correlation_id, occurred_at, published_at)
		VALUES ($1, gen_random_uuid(), 'WalletBalanceChanged', 1, '{}', 'c', now() - make_interval(secs => $2), `+publishedAt+`)`, id, age.Seconds()+1))
	return id
}

func (f retentionFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	require.NoError(t, f.s.owner.QueryRow(f.s.ctx, `SELECT count(*) FROM `+table).Scan(&n))
	return n
}

const day = 24 * time.Hour

func TestOnlyCompletedMessagesOlderThanTheRetentionAreForgotten(t *testing.T) {
	f := newRetentionFixture(t)
	f.message(t, "old-1", 20*day, true)
	f.message(t, "old-2", 15*day, true)
	f.message(t, "recent", 10*day, true)
	f.message(t, "in-hand", 30*day, false)

	removed, err := f.store.PruneInbox(t.Context(), 14*day, 100)

	require.NoError(t, err)
	require.Equal(t, 2, removed)
	var left []string
	rows, err := f.s.owner.Query(f.s.ctx, `SELECT message_id FROM inbox ORDER BY message_id`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		left = append(left, id)
	}
	require.Equal(t, []string{"in-hand", "recent"}, left, "a message still in hand is never forgotten")
}

func TestOnlyPublishedEventsOlderThanTheRetentionAreRemoved(t *testing.T) {
	f := newRetentionFixture(t)
	f.event(t, 10*day, true)
	recent := f.event(t, 2*day, true)
	pending := f.event(t, 30*day, false)

	removed, err := f.store.PruneOutbox(t.Context(), 7*day, 100)

	require.NoError(t, err)
	require.Equal(t, 1, removed)
	var left []uuid.UUID
	rows, err := f.s.owner.Query(f.s.ctx, `SELECT event_id FROM outbox`)
	require.NoError(t, err)
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		left = append(left, id)
	}
	require.ElementsMatch(t, []uuid.UUID{recent, pending}, left, "an event not yet published is never removed, however old")
}

func TestPruningRemovesAtMostABatch(t *testing.T) {
	f := newRetentionFixture(t)
	for i := range 5 {
		f.message(t, fmt.Sprintf("m-%d", i), 30*day, true)
	}
	removed, err := f.store.PruneInbox(t.Context(), 14*day, 2)
	require.NoError(t, err)
	require.Equal(t, 2, removed)
	require.Equal(t, 3, f.count(t, "inbox"))
}

// The floors hold whatever the application asks: below them the database refuses.
func TestTheDatabaseRefusesToForgetTooEarly(t *testing.T) {
	f := newRetentionFixture(t)
	f.message(t, "five-days", 5*day, true)
	f.event(t, 2*time.Hour, true)

	_, err := f.store.PruneInbox(t.Context(), 6*day, 100)
	require.ErrorContains(t, err, "at least 7 days")
	_, err = f.store.PruneOutbox(t.Context(), time.Hour, 100)
	require.ErrorContains(t, err, "at least 1 day")
	_, err = f.store.PruneInbox(t.Context(), 14*day, 0)
	require.ErrorContains(t, err, "max_rows")
	require.Equal(t, 1, f.count(t, "inbox"))
	require.Equal(t, 1, f.count(t, "outbox"))

	// Called directly, as the role, the same.
	require.Equal(t, checkViolation, f.s.exec(f.s.app, `SELECT prune_inbox(interval '1 day', 10)`))
	require.Equal(t, checkViolation, f.s.exec(f.s.app, `SELECT prune_outbox(interval '1 minute', 10)`))
}

// The application role removes rows only through the functions.
func TestTheApplicationRoleCannotDeleteFromTheInboxOrTheOutboxDirectly(t *testing.T) {
	f := newRetentionFixture(t)
	f.message(t, "m", 30*day, true)
	id := f.event(t, 30*day, true)

	require.Equal(t, insufficientPrivilege, f.s.exec(f.s.app, `DELETE FROM inbox`))
	require.Equal(t, insufficientPrivilege, f.s.exec(f.s.app, `DELETE FROM outbox WHERE event_id = $1`, id))
	require.Equal(t, insufficientPrivilege, f.s.exec(f.s.app, `TRUNCATE outbox`))
	require.Equal(t, 1, f.count(t, "inbox"))
	require.Equal(t, 1, f.count(t, "outbox"))
}
