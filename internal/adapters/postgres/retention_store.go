package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

// RetentionStore calls the retention functions of migration 000013, the only way the
// application role can remove inbox or outbox rows. It implements application.RetentionStore.
type RetentionStore struct {
	pool *pgxpool.Pool
}

func NewRetentionStore(pool *pgxpool.Pool) *RetentionStore { return &RetentionStore{pool: pool} }

var _ application.RetentionStore = (*RetentionStore)(nil)

func (s *RetentionStore) PruneInbox(ctx context.Context, olderThan time.Duration, maxRows int) (int, error) {
	return s.prune(ctx, "prune_inbox", olderThan, maxRows)
}

func (s *RetentionStore) PruneOutbox(ctx context.Context, olderThan time.Duration, maxRows int) (int, error) {
	return s.prune(ctx, "prune_outbox", olderThan, maxRows)
}

func (s *RetentionStore) prune(ctx context.Context, function string, olderThan time.Duration, maxRows int) (int, error) {
	var removed int
	// The function name is one of two constants above, never input.
	if err := s.pool.QueryRow(ctx, `SELECT `+function+`(make_interval(secs => $1), $2)`, olderThan.Seconds(), maxRows).Scan(&removed); err != nil {
		return 0, markTransient(fmt.Errorf("%s: %w", function, err))
	}
	return removed, nil
}
