package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

// PendingStore is the worker's view of the operations waiting for a Reference. It
// implements application.PendingStore. Every time it uses is the database clock.
type PendingStore struct {
	pool *pgxpool.Pool
}

func NewPendingStore(pool *pgxpool.Pool) *PendingStore { return &PendingStore{pool: pool} }

var _ application.PendingStore = (*PendingStore)(nil)

// reservePendingSQL leases the operations that are due. SKIP LOCKED lets any number of
// workers run it at once without waiting for each other or taking the same row. A PENDING
// row can only be one whose acceptance never finished (the request that made it commits
// only a decided state), so it is due at once.
const reservePendingSQL = `
WITH due AS (
	SELECT id FROM wager_transactions
	WHERE origin = 'EXTERNAL'
	  AND (status = 'PENDING' OR (status = 'PENDING_REFERENCE' AND next_attempt_at <= now()))
	  AND (locked_until IS NULL OR locked_until <= now())
	ORDER BY coalesce(next_attempt_at, created_at)
	LIMIT $1
	FOR UPDATE SKIP LOCKED)
UPDATE wager_transactions t
SET locked_until = now() + make_interval(secs => $2)
FROM due
WHERE t.id = due.id
RETURNING t.id, coalesce(t.wallet_id, t.requested_wallet_id)`

func (s *PendingStore) Reserve(ctx context.Context, limit int, lease time.Duration) ([]application.PendingItem, error) {
	rows, err := s.pool.Query(ctx, reservePendingSQL, limit, lease.Seconds())
	if err != nil {
		return nil, markTransient(fmt.Errorf("reserve pending operations: %w", err))
	}
	defer rows.Close()
	var items []application.PendingItem
	for rows.Next() {
		var item application.PendingItem
		if err := rows.Scan(&item.TransactionID, &item.WalletID); err != nil {
			return nil, fmt.Errorf("read pending operation: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, markTransient(fmt.Errorf("reserve pending operations: %w", err))
	}
	return items, nil
}

func (s *PendingStore) Release(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE wager_transactions SET locked_until = NULL WHERE id = ANY($1) AND status IN ('PENDING', 'PENDING_REFERENCE')`, ids); err != nil {
		return markTransient(fmt.Errorf("release pending operations: %w", err))
	}
	return nil
}

func (s *PendingStore) Stats(ctx context.Context) (application.PendingStats, error) {
	var count int
	var oldest float64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
		FROM wager_transactions WHERE status = 'PENDING_REFERENCE'`).Scan(&count, &oldest)
	if err != nil {
		return application.PendingStats{}, markTransient(fmt.Errorf("count pending operations: %w", err))
	}
	return application.PendingStats{Waiting: count, Oldest: time.Duration(oldest * float64(time.Second))}, nil
}

func (s *PendingStore) RecordError(ctx context.Context, id uuid.UUID, reason string, base, ceiling time.Duration) (int, error) {
	reason = strings.ToValidUTF8(reason, "?")
	if len(reason) > maxReasonLength {
		reason = reason[:maxReasonLength]
	}
	if reason == "" {
		reason = "unknown"
	}
	var count int
	err := s.pool.QueryRow(ctx, `
		UPDATE wager_transactions
		SET error_count = error_count + 1, last_error = $2,
		    locked_until = now() + make_interval(secs => least($3 * power(2, least(error_count, 30)), $4))
		WHERE id = $1 AND status IN ('PENDING', 'PENDING_REFERENCE')
		RETURNING error_count`, id, reason, base.Seconds(), ceiling.Seconds()).Scan(&count)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // decided meanwhile: nothing to count
	}
	if err != nil {
		return 0, markTransient(fmt.Errorf("record the error of pending operation %s: %w", id, err))
	}
	return count, nil
}

func (s *PendingStore) Fail(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE wager_transactions
		SET status = 'FAILED', failure_code = 'INTERNAL_PERMANENT_FAILURE', updated_at = now(), processed_at = now(), locked_until = NULL
		WHERE id = $1 AND status IN ('PENDING', 'PENDING_REFERENCE')`, id)
	if err != nil {
		return false, markTransient(fmt.Errorf("fail pending operation %s: %w", id, err))
	}
	return tag.RowsAffected() == 1, nil
}
