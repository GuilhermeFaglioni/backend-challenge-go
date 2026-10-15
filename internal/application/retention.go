package application

import (
	"context"
	"time"
)

// RetentionStore removes inbox and outbox rows that have done their job. The database
// enforces the floors (a message is remembered for at least 7 days, a published event is
// kept for at least 1 day) whatever the caller asks.
type RetentionStore interface {
	// PruneInbox removes up to maxRows completed messages older than olderThan and says
	// how many it removed.
	PruneInbox(ctx context.Context, olderThan time.Duration, maxRows int) (int, error)
	// PruneOutbox removes up to maxRows published events older than olderThan.
	PruneOutbox(ctx context.Context, olderThan time.Duration, maxRows int) (int, error)
}
