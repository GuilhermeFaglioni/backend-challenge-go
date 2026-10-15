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
	// Reserve leases up to limit events that are due and returns them in the order
	// they were written. It never returns an event another publisher holds, and an
	// event of a wallet only after every earlier event of that wallet is published,
	// so a wallet's events are published in order. A lease that ran out makes the
	// event available again.
	Reserve(ctx context.Context, limit int, lease time.Duration) ([]OutboxEntry, error)
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
