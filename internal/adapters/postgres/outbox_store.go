package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

// maxReasonLength bounds the error text kept on an event; the column is for a person
// to read, not a log.
const maxReasonLength = 500

// OutboxStore is the publisher's view of the outbox table. It implements
// application.OutboxStore. Every time it uses is the database clock.
type OutboxStore struct {
	pool *pgxpool.Pool
}

func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore { return &OutboxStore{pool: pool} }

var _ application.OutboxStore = (*OutboxStore)(nil)

// reserveSQL leases the oldest due wallets and a run of their events. A wallet's head is
// its oldest unpublished event; the NOT EXISTS makes a head wait for every earlier
// unpublished event of its wallet (including ones leased by another publisher), which is
// what keeps a wallet in one publisher's hands at a time and its events in order. SKIP
// LOCKED lets any number of publishers run it at once without waiting for each other or
// taking the same head. The chain then adds the events that follow each head, up to
// depth: nobody else can hold them, because none of them is a head while the head is
// unpublished.
const reserveSQL = `
WITH heads AS (
	SELECT e.event_id, e.aggregate_id, e.seq
	FROM outbox e
	WHERE e.published_at IS NULL
	  AND e.next_attempt_at <= now()
	  AND (e.locked_until IS NULL OR e.locked_until <= now())
	  AND NOT EXISTS (
		SELECT 1 FROM outbox earlier
		WHERE earlier.aggregate_id = e.aggregate_id
		  AND earlier.published_at IS NULL
		  AND earlier.seq < e.seq)
	ORDER BY e.seq
	LIMIT $1
	FOR UPDATE OF e SKIP LOCKED),
chain AS (
	SELECT c.event_id
	FROM heads h
	CROSS JOIN LATERAL (
		SELECT o.event_id FROM outbox o
		WHERE o.aggregate_id = h.aggregate_id AND o.published_at IS NULL AND o.seq >= h.seq
		ORDER BY o.seq
		LIMIT $3) c)
UPDATE outbox o
SET locked_until = now() + make_interval(secs => $2)
FROM chain
WHERE o.event_id = chain.event_id
RETURNING o.seq, o.event_id, o.aggregate_id, o.event_type, o.version, o.payload,
          o.correlation_id, o.causation_id, o.occurred_at, o.attempts`

func (s *OutboxStore) Reserve(ctx context.Context, wallets, depth int, lease time.Duration) ([]application.OutboxEntry, error) {
	rows, err := s.pool.Query(ctx, reserveSQL, wallets, lease.Seconds(), max(depth, 1))
	if err != nil {
		return nil, markTransient(fmt.Errorf("reserve outbox events: %w", err))
	}
	defer rows.Close()

	type reserved struct {
		seq   int64
		entry application.OutboxEntry
	}
	var found []reserved
	for rows.Next() {
		var r reserved
		var payload []byte
		var correlation, causation *string
		if err := rows.Scan(&r.seq, &r.entry.EventID, &r.entry.AggregateID, &r.entry.EventType, &r.entry.Version, &payload,
			&correlation, &causation, &r.entry.OccurredAt, &r.entry.Attempts); err != nil {
			return nil, fmt.Errorf("read outbox event: %w", err)
		}
		r.entry.Payload = json.RawMessage(payload)
		if correlation != nil {
			r.entry.CorrelationID = *correlation
		}
		if causation != nil {
			r.entry.CausationID = *causation
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, markTransient(fmt.Errorf("reserve outbox events: %w", err))
	}

	// RETURNING does not promise an order; the write order is the seq.
	slices.SortFunc(found, func(a, b reserved) int {
		switch {
		case a.seq < b.seq:
			return -1
		case a.seq > b.seq:
			return 1
		}
		return 0
	})
	entries := make([]application.OutboxEntry, len(found))
	for i, r := range found {
		entries[i] = r.entry
	}
	return entries, nil
}

func (s *OutboxStore) MarkPublished(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox SET published_at = now(), locked_until = NULL, last_error = NULL
		WHERE event_id = ANY($1) AND published_at IS NULL`, ids)
	if err != nil {
		return markTransient(fmt.Errorf("mark outbox events published: %w", err))
	}
	return nil
}

func (s *OutboxStore) MarkFailed(ctx context.Context, failures []application.OutboxFailure) error {
	if len(failures) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, f := range failures {
		batch.Queue(`
			UPDATE outbox
			SET attempts = attempts + 1,
			    next_attempt_at = now() + make_interval(secs => $2),
			    locked_until = NULL,
			    last_error = $3
			WHERE event_id = $1 AND published_at IS NULL`,
			f.EventID, f.RetryIn.Seconds(), cleanReason(f.Reason, maxReasonLength))
	}
	results := s.pool.SendBatch(ctx, batch)
	for range failures {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return markTransient(fmt.Errorf("mark outbox events failed: %w", err))
		}
	}
	if err := results.Close(); err != nil {
		return markTransient(fmt.Errorf("mark outbox events failed: %w", err))
	}
	return nil
}

func (s *OutboxStore) Release(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE outbox SET locked_until = NULL WHERE event_id = ANY($1) AND published_at IS NULL`, ids)
	if err != nil {
		return markTransient(fmt.Errorf("release outbox events: %w", err))
	}
	return nil
}

func (s *OutboxStore) OldestUnpublished(ctx context.Context) (time.Duration, bool, error) {
	var seconds *float64
	err := s.pool.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM now() - min(occurred_at))::float8 FROM outbox WHERE published_at IS NULL`).Scan(&seconds)
	if err != nil {
		return 0, false, markTransient(fmt.Errorf("read the outbox lag: %w", err))
	}
	if seconds == nil {
		return 0, false, nil
	}
	return max(time.Duration(*seconds*float64(time.Second)), 0), true, nil
}

// cleanReason makes an error text storable: PostgreSQL text cannot hold NUL bytes or
// invalid UTF-8, and a failed publication must not become a failed bookkeeping. It is
// cut to at most n bytes without splitting a character.
func cleanReason(s string, n int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "?")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
