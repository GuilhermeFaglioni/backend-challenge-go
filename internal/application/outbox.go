package application

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// OutboxEntry is an event written to the outbox, waiting to be published, as it was
// stored. Payload is the event's data, the immutable snapshot.
type OutboxEntry struct {
	EventID       uuid.UUID
	AggregateID   uuid.UUID
	EventType     string
	Version       int
	Payload       json.RawMessage
	CorrelationID string
	CausationID   string // empty when there is none
	OccurredAt    time.Time
	Attempts      int // failed attempts so far
}

// OutboxFailure says why publishing an entry failed and how long to wait before the
// next attempt.
type OutboxFailure struct {
	EventID uuid.UUID
	Reason  string
	RetryIn time.Duration
}

// OutboxStore is what the publisher needs from the outbox table. All times come from
// the database clock.
type OutboxStore interface {
	// Reserve leases the due events of up to `wallets` wallets, at most `depth`
	// consecutive events of each, and returns them in the order they were written. A
	// wallet is only taken when every earlier event of it is published, so it is in the
	// hands of one publisher at a time and its events go out in order; its events after
	// the first are the ones that come right after it. It never returns an event another
	// publisher holds; a lease that ran out makes the event available again.
	Reserve(ctx context.Context, wallets, depth int, lease time.Duration) ([]OutboxEntry, error)
	// MarkPublished records that the events were accepted by the queue.
	MarkPublished(ctx context.Context, ids []uuid.UUID) error
	// MarkFailed counts a failed attempt, releases the lease and schedules the next.
	MarkFailed(ctx context.Context, failures []OutboxFailure) error
	// Release gives the leases back without counting an attempt.
	Release(ctx context.Context, ids []uuid.UUID) error
	// OldestUnpublished is the age of the oldest event not yet published; false when
	// there is none.
	OldestUnpublished(ctx context.Context) (time.Duration, bool, error)
}

// OutboundMessage is an event ready to be sent to the events queue.
type OutboundMessage struct {
	EventID         uuid.UUID
	GroupID         string // the wallet: events of one wallet are delivered in order
	DeduplicationID string // the event id: a repeated publication is recognised
	EventType       string
	CorrelationID   string
	Body            []byte
}

// MessageSender sends a batch to the events queue. The returned map holds, by event
// id, the entries the queue refused; an error means the whole batch failed.
type MessageSender interface {
	Send(ctx context.Context, messages []OutboundMessage) (refused map[uuid.UUID]error, err error)
}

// RetryReason says why a unit of work ran again.
type RetryReason string

const (
	// RetrySerializationFailure: PostgreSQL aborted the transaction (40001).
	RetrySerializationFailure RetryReason = "serialization_failure"
	// RetryDeadlock: PostgreSQL broke a deadlock by aborting this transaction (40P01).
	RetryDeadlock RetryReason = "deadlock"
	// RetryConcurrentUpdate: the wallet changed under the unit of work (ErrConcurrentUpdate).
	RetryConcurrentUpdate RetryReason = "concurrent_update"
)

// RetryReasons lists every RetryReason, so a metric can start each at zero.
var RetryReasons = []RetryReason{RetrySerializationFailure, RetryDeadlock, RetryConcurrentUpdate}

// RetryObserver is told each time a unit of work is retried: each is a concurrency
// conflict that the retry absorbed.
type RetryObserver interface {
	Retried(RetryReason)
}
