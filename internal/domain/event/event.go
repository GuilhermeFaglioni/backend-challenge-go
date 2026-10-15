// Package event defines the integration events and their envelope. An Event is an
// immutable snapshot: the constructor of each kind fixes its type and version, and
// what is serialized is exactly what was validated. See ARCHITECTURE.md, section 12.
package event

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

const (
	typeWagerTransactionProcessed = "WagerTransactionProcessed"
	typeWagerTransactionRejected  = "WagerTransactionRejected"
	typeWagerTransactionPending   = "WagerTransactionPendingReference"
	typeWalletBalanceChanged      = "WalletBalanceChanged"
)

// timeLayout is RFC 3339 in UTC with milliseconds.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// Meta is what the caller supplies for the envelope of any event.
type Meta struct {
	EventID       uuid.UUID
	CorrelationID string
	CausationID   string // optional
	OccurredAt    time.Time
}

// Data is the typed payload of an event. Only this package implements it, so the
// type and the version of an event cannot be chosen by the caller.
type Data interface {
	eventType() string
	eventVersion() int
	aggregateID() uuid.UUID
}

// Event is the envelope and its data. The zero value is invalid.
type Event struct {
	eventID       uuid.UUID
	correlationID string
	causationID   string
	occurredAt    time.Time
	data          Data
}

func newEvent(m Meta, d Data) (Event, error) {
	switch {
	case m.EventID == uuid.Nil:
		return Event{}, ErrInvalidID
	case m.CorrelationID == "":
		return Event{}, ErrInvalidCorrelation
	case m.OccurredAt.IsZero():
		return Event{}, ErrInvalidTime
	}
	return Event{
		eventID: m.EventID, correlationID: m.CorrelationID, causationID: m.CausationID,
		occurredAt: m.OccurredAt.UTC().Truncate(time.Millisecond), data: d,
	}, nil
}

func (e Event) IsValid() bool         { return e.data != nil }
func (e Event) EventID() uuid.UUID    { return e.eventID }
func (e Event) CorrelationID() string { return e.correlationID }
func (e Event) CausationID() string   { return e.causationID }
func (e Event) OccurredAt() time.Time { return e.occurredAt }
func (e Event) Data() Data            { return e.data }

// Type is the event type, for example "WalletBalanceChanged".
func (e Event) Type() string {
	if e.data == nil {
		return ""
	}
	return e.data.eventType()
}

// Version is the version of the event's schema.
func (e Event) Version() int {
	if e.data == nil {
		return 0
	}
	return e.data.eventVersion()
}

// AggregateID is the wallet the event is about; events are ordered per wallet.
func (e Event) AggregateID() uuid.UUID {
	if e.data == nil {
		return uuid.Nil
	}
	return e.data.aggregateID()
}

// DataJSON is the serialized data alone: the immutable snapshot the outbox stores.
func (e Event) DataJSON() ([]byte, error) {
	if e.data == nil {
		return nil, ErrUninitialized
	}
	return json.Marshal(e.data)
}

// MarshalJSON writes the envelope with its fields in a fixed order.
func (e Event) MarshalJSON() ([]byte, error) {
	if e.data == nil {
		return nil, ErrUninitialized
	}
	data, err := e.DataJSON()
	if err != nil {
		return nil, err
	}
	return Envelope{
		EventID: e.eventID, EventType: e.Type(), AggregateID: e.AggregateID(),
		CorrelationID: e.correlationID, CausationID: e.causationID,
		OccurredAt: e.occurredAt, Version: e.Version(), Data: data,
	}.MarshalJSON()
}

// Envelope is the stored and published form of an event: the envelope fields and the
// data exactly as it was serialized when the event was written. The publisher builds
// it from an outbox row, so what it sends is byte for byte what Event.MarshalJSON
// produces, and what was stored is what was sent.
type Envelope struct {
	EventID       uuid.UUID
	EventType     string
	AggregateID   uuid.UUID
	CorrelationID string
	CausationID   string // optional
	OccurredAt    time.Time
	Version       int
	Data          json.RawMessage
}

// MarshalJSON writes the envelope with its fields in a fixed order. It is the one
// place the wire format is defined.
func (e Envelope) MarshalJSON() ([]byte, error) {
	if len(e.Data) == 0 {
		return nil, ErrUninitialized
	}
	type wire struct {
		EventID       uuid.UUID       `json:"eventId"`
		EventType     string          `json:"eventType"`
		AggregateID   uuid.UUID       `json:"aggregateId"`
		CorrelationID string          `json:"correlationId"`
		CausationID   string          `json:"causationId,omitempty"`
		OccurredAt    string          `json:"occurredAt"`
		Version       int             `json:"version"`
		Data          json.RawMessage `json:"data"`
	}
	return json.Marshal(wire{
		EventID: e.EventID, EventType: e.EventType, AggregateID: e.AggregateID,
		CorrelationID: e.CorrelationID, CausationID: e.CausationID,
		OccurredAt: e.OccurredAt.UTC().Format(timeLayout), Version: e.Version, Data: e.Data,
	})
}

// --- WagerTransactionProcessed -----------------------------------------------------

// WagerTransactionProcessedData says that a Wager Transaction ended PROCESSED. An
// internal opening carries no provider, external id, round or game; an external
// transaction carries all four.
type WagerTransactionProcessedData struct {
	TransactionID         uuid.UUID    `json:"transactionId"`
	WalletID              uuid.UUID    `json:"walletId"`
	PlayerID              uuid.UUID    `json:"playerId"`
	Kind                  wager.Kind   `json:"kind"`
	Origin                wager.Origin `json:"origin"`
	ProviderID            string       `json:"providerId,omitempty"`
	ExternalTransactionID string       `json:"externalTransactionId,omitempty"`
	RoundID               string       `json:"roundId,omitempty"`
	GameID                string       `json:"gameId,omitempty"`
	// ReferenceExternalTransactionID is the Reference the operation named, when it named one.
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId,omitempty"`
	Money                          money.Money `json:"money"`
	Balance                        money.Money `json:"balance"`
	WalletVersion                  int64       `json:"walletVersion"`
}

func (WagerTransactionProcessedData) eventType() string        { return typeWagerTransactionProcessed }
func (WagerTransactionProcessedData) eventVersion() int        { return 1 }
func (d WagerTransactionProcessedData) aggregateID() uuid.UUID { return d.WalletID }

// NewWagerTransactionProcessed builds the event, refusing data that does not hold
// together.
func NewWagerTransactionProcessed(m Meta, d WagerTransactionProcessedData) (Event, error) {
	switch {
	case d.TransactionID == uuid.Nil || d.WalletID == uuid.Nil || d.PlayerID == uuid.Nil:
		return Event{}, ErrInvalidID
	case d.Kind == "" || d.Origin == "":
		return Event{}, ErrInvalidKind
	case !hasExactlyTheMetadataOf(d.Origin, d.ProviderID, d.ExternalTransactionID, d.RoundID, d.GameID):
		return Event{}, fmt.Errorf("%w: the provider's metadata goes with an external origin, and only with it", ErrInvalidKind)
	case !d.Money.IsValid() || d.Money.IsNegative() || !d.Balance.IsValid() || d.Balance.IsNegative():
		return Event{}, ErrInvalidMoney
	case d.WalletVersion < 1:
		return Event{}, ErrInvalidVersion
	}
	if _, err := d.Money.Compare(d.Balance); err != nil {
		return Event{}, fmt.Errorf("event: %w", err)
	}
	return newEvent(m, d)
}

// hasExactlyTheMetadataOf: an external origin carries all four fields, an internal
// one none of them.
func hasExactlyTheMetadataOf(origin wager.Origin, fields ...string) bool {
	present := 0
	for _, f := range fields {
		if f != "" {
			present++
		}
	}
	if origin == wager.OriginExternal {
		return present == len(fields)
	}
	return present == 0
}

// --- WagerTransactionRejected ------------------------------------------------------

// WagerTransactionRejectedData says that a business rule refused an external
// operation. The wallet is the one the operation asked for, which may not exist
// (WALLET_NOT_FOUND).
type WagerTransactionRejectedData struct {
	TransactionID         uuid.UUID  `json:"transactionId"`
	WalletID              uuid.UUID  `json:"walletId"`
	PlayerID              uuid.UUID  `json:"playerId"`
	Kind                  wager.Kind `json:"kind"`
	ProviderID            string     `json:"providerId"`
	ExternalTransactionID string     `json:"externalTransactionId"`
	RoundID               string     `json:"roundId"`
	GameID                string     `json:"gameId"`
	// ReferenceExternalTransactionID is the Reference the operation named, when it named one.
	ReferenceExternalTransactionID string            `json:"referenceExternalTransactionId,omitempty"`
	Money                          money.Money       `json:"money"`
	FailureCode                    wager.FailureCode `json:"failureCode"`
}

func (WagerTransactionRejectedData) eventType() string        { return typeWagerTransactionRejected }
func (WagerTransactionRejectedData) eventVersion() int        { return 1 }
func (d WagerTransactionRejectedData) aggregateID() uuid.UUID { return d.WalletID }

// NewWagerTransactionRejected builds the event, refusing data that does not hold
// together.
func NewWagerTransactionRejected(m Meta, d WagerTransactionRejectedData) (Event, error) {
	switch {
	case d.TransactionID == uuid.Nil || d.WalletID == uuid.Nil || d.PlayerID == uuid.Nil:
		return Event{}, ErrInvalidID
	case d.Kind == "" || d.Kind == wager.KindOpening:
		return Event{}, ErrInvalidKind
	case !hasExactlyTheMetadataOf(wager.OriginExternal, d.ProviderID, d.ExternalTransactionID, d.RoundID, d.GameID):
		return Event{}, fmt.Errorf("%w: a rejected operation always comes from a provider", ErrInvalidKind)
	case !d.Money.IsValid() || d.Money.IsNegative():
		return Event{}, ErrInvalidMoney
	case !d.FailureCode.IsRejection():
		return Event{}, ErrInvalidFailureCode
	}
	return newEvent(m, d)
}

// --- WagerTransactionPendingReference ----------------------------------------------

// WagerTransactionPendingReferenceData says that an external operation is waiting for
// the Reference it names, which has not arrived. It ends later in WagerTransactionProcessed
// or WagerTransactionRejected for the same transaction.
type WagerTransactionPendingReferenceData struct {
	TransactionID                  uuid.UUID   `json:"transactionId"`
	WalletID                       uuid.UUID   `json:"walletId"`
	PlayerID                       uuid.UUID   `json:"playerId"`
	Kind                           wager.Kind  `json:"kind"`
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
	Money                          money.Money `json:"money"`
	// ExpiresAt is when the wait ends: the operation is then refused if its Reference
	// has not come.
	ExpiresAt time.Time `json:"expiresAt"`
}

func (WagerTransactionPendingReferenceData) eventType() string { return typeWagerTransactionPending }
func (WagerTransactionPendingReferenceData) eventVersion() int { return 1 }
func (d WagerTransactionPendingReferenceData) aggregateID() uuid.UUID {
	return d.WalletID
}

// MarshalJSON writes the expiry in the envelope's time format.
func (d WagerTransactionPendingReferenceData) MarshalJSON() ([]byte, error) {
	type plain WagerTransactionPendingReferenceData
	return json.Marshal(struct {
		plain
		ExpiresAt string `json:"expiresAt"`
	}{plain: plain(d), ExpiresAt: d.ExpiresAt.UTC().Format(timeLayout)})
}

// NewWagerTransactionPendingReference builds the event, refusing data that does not hold
// together.
func NewWagerTransactionPendingReference(m Meta, d WagerTransactionPendingReferenceData) (Event, error) {
	switch {
	case d.TransactionID == uuid.Nil || d.WalletID == uuid.Nil || d.PlayerID == uuid.Nil:
		return Event{}, ErrInvalidID
	case d.Kind == "" || d.Kind == wager.KindOpening:
		return Event{}, ErrInvalidKind
	case !hasExactlyTheMetadataOf(wager.OriginExternal, d.ProviderID, d.ExternalTransactionID, d.RoundID, d.GameID, d.ReferenceExternalTransactionID):
		return Event{}, fmt.Errorf("%w: a waiting operation always comes from a provider and names a reference", ErrInvalidKind)
	case !d.Money.IsValid() || d.Money.IsNegative():
		return Event{}, ErrInvalidMoney
	case d.ExpiresAt.IsZero() || !d.ExpiresAt.After(m.OccurredAt):
		return Event{}, ErrInvalidTime
	}
	return newEvent(m, d)
}

// --- WalletBalanceChanged ----------------------------------------------------------

// WalletBalanceChangedData says that a wallet's balance moved, with the Ledger Entry
// that explains it.
type WalletBalanceChangedData struct {
	WalletID      uuid.UUID        `json:"walletId"`
	TransactionID uuid.UUID        `json:"transactionId"`
	Direction     wallet.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

func (WalletBalanceChangedData) eventType() string        { return typeWalletBalanceChanged }
func (WalletBalanceChangedData) eventVersion() int        { return 1 }
func (d WalletBalanceChangedData) aggregateID() uuid.UUID { return d.WalletID }

// NewWalletBalanceChanged builds the event, checking that the balances follow from
// the amount and the direction.
func NewWalletBalanceChanged(m Meta, d WalletBalanceChangedData) (Event, error) {
	switch {
	case d.WalletID == uuid.Nil || d.TransactionID == uuid.Nil:
		return Event{}, ErrInvalidID
	case d.Direction != wallet.DirectionDebit && d.Direction != wallet.DirectionCredit:
		return Event{}, ErrInvalidDirection
	case !d.Money.IsPositive():
		return Event{}, ErrInvalidMoney
	case !d.BalanceBefore.IsValid() || d.BalanceBefore.IsNegative() || !d.BalanceAfter.IsValid() || d.BalanceAfter.IsNegative():
		return Event{}, ErrInvalidMoney
	case d.WalletVersion < 1:
		return Event{}, ErrInvalidVersion
	}

	var expected money.Money
	var err error
	if d.Direction == wallet.DirectionCredit {
		expected, err = d.BalanceBefore.Add(d.Money)
	} else {
		expected, err = d.BalanceBefore.Sub(d.Money)
	}
	if err != nil {
		return Event{}, fmt.Errorf("event: %w", err)
	}
	same, err := expected.Compare(d.BalanceAfter)
	if err != nil {
		return Event{}, fmt.Errorf("event: %w", err)
	}
	if same != 0 {
		return Event{}, ErrInconsistentData
	}
	return newEvent(m, d)
}
