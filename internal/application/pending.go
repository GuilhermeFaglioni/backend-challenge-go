package application

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
)

// PendingPolicy says how long and how often an operation waits for its Reference
// (ARCHITECTURE.md, section 10): the gaps between attempts grow exponentially with some
// jitter, and the wait ends after a number of attempts or a time, whichever comes first.
type PendingPolicy struct {
	MaxAttempts int           // attempts, the first one (when the operation arrives) included
	TTL         time.Duration // how long after the operation arrived it may still wait
	BackoffBase time.Duration // the gap after the first attempt
	BackoffMax  time.Duration // the gap never grows beyond this
	// Jitter returns a number in [0,1); nil means a random one. For tests.
	Jitter func() float64
}

// jitterSpread is how far a gap may stray from its nominal value, so operations that
// started waiting together do not try together.
const jitterSpread = 0.2

// Gap is how long to wait after the given number of attempts: the base, doubled for each
// attempt after the first, up to the maximum, then spread by ±20%.
func (p PendingPolicy) Gap(attempts int) time.Duration {
	exp := math.Min(float64(max(attempts-1, 0)), 30)
	gap := math.Min(float64(p.BackoffBase)*math.Pow(2, exp), float64(p.BackoffMax))
	jitter := p.Jitter
	if jitter == nil {
		jitter = rand.Float64 //nolint:gosec // spreading retries needs no secure randomness
	}
	gap *= 1 + jitterSpread*(2*jitter()-1)
	return time.Duration(gap)
}

// ExpiresAt is when an operation that arrived at the given time stops waiting.
func (p PendingPolicy) ExpiresAt(arrived time.Time) time.Time { return arrived.Add(p.TTL) }

// NextAttempt is when to try again after `attempts` attempts, never later than the end of
// the wait: the last attempt falls on the expiry, so an operation is refused when it is
// due and not whenever a worker comes by.
func (p PendingPolicy) NextAttempt(now time.Time, attempts int, expiresAt time.Time) time.Time {
	next := now.Add(p.Gap(attempts))
	if next.After(expiresAt) {
		return expiresAt
	}
	return next
}

// Exhausted reports whether the operation may not wait any more after this attempt.
func (p PendingPolicy) Exhausted(attempts int, now, expiresAt time.Time) bool {
	return attempts >= p.MaxAttempts || !now.Before(expiresAt)
}

// PendingSchedule is where a waiting operation stands: how many attempts were made, when
// the next one is due and when the wait ends.
type PendingSchedule struct {
	Attempts      int
	NextAttemptAt time.Time
	ExpiresAt     time.Time
}

// PendingItem is a waiting operation reserved by a worker.
type PendingItem struct {
	TransactionID uuid.UUID
	WalletID      uuid.UUID // the wallet the operation was accepted on
}

// PendingStats is what the metrics show of the operations waiting for a Reference.
type PendingStats struct {
	Waiting int           // operations in PENDING_REFERENCE
	Oldest  time.Duration // how long the one that has waited longest has waited
}

// PendingStore is what the worker needs from the table, outside any unit of work. All
// times are the database's clock.
type PendingStore interface {
	// Reserve leases up to limit operations that are due: PENDING_REFERENCE ones whose
	// next attempt has come, and PENDING ones, which are an acceptance that never
	// finished. It never returns one another worker holds; a lease that ran out makes the
	// operation available again.
	Reserve(ctx context.Context, limit int, lease time.Duration) ([]PendingItem, error)
	// Release gives the leases back, for work that was reserved and not done.
	Release(ctx context.Context, ids []uuid.UUID) error
	// Stats counts the waiting operations.
	Stats(ctx context.Context) (PendingStats, error)
	// RecordError counts a try that could not be made for a reason that is not an
	// outage, keeps the reason and holds the operation back: base after the first error,
	// doubling with each one, up to ceiling. It returns how many such errors the operation
	// has had (0 if it was decided meanwhile).
	RecordError(ctx context.Context, id uuid.UUID, reason string, base, ceiling time.Duration) (int, error)
	// Fail ends an operation that keeps failing as FAILED, with the failure code
	// INTERNAL_PERMANENT_FAILURE. It does so in SQL alone, because the row may be one the
	// application cannot even read. It reports whether the operation was still undecided.
	Fail(ctx context.Context, id uuid.UUID) (bool, error)
}
