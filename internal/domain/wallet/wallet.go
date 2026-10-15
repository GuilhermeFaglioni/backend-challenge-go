package wallet

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

// Wallet is a player's balance in one currency. It is an immutable value: Debit and
// Credit return the next state and the Ledger Entry that explains it, and a refused
// movement returns nothing usable. The zero value is invalid.
type Wallet struct {
	id, playerID      uuid.UUID
	balance           money.Money
	version           int64
	lastTransactionID uuid.UUID
	createdAt         time.Time
	updatedAt         time.Time
}

// Movement is one debit or credit of a wallet, caused by a Wager Transaction.
type Movement struct {
	EntryID       uuid.UUID
	TransactionID uuid.UUID
	Amount        money.Money
	At            time.Time
}

// State is a stored wallet, for Rehydrate.
type State struct {
	ID, PlayerID      uuid.UUID
	Balance           money.Money
	Version           int64
	LastTransactionID uuid.UUID // uuid.Nil when the wallet has never moved
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Open creates an empty wallet at version 1.
func Open(id, playerID uuid.UUID, currency money.Currency, now time.Time) (Wallet, error) {
	zero, err := money.Zero(currency)
	if err != nil {
		return Wallet{}, err
	}
	if id == uuid.Nil || playerID == uuid.Nil {
		return Wallet{}, ErrInvalidID
	}
	if now.IsZero() {
		return Wallet{}, ErrInvalidTime
	}
	now = now.UTC()
	return Wallet{id: id, playerID: playerID, balance: zero, version: 1, createdAt: now, updatedAt: now}, nil
}

// OpenWithBalance creates a wallet funded by its opening Wager Transaction, at
// version 1, together with the credit entry (balance 0 to the initial balance).
func OpenWithBalance(id, playerID uuid.UUID, initial money.Money, openingTransactionID uuid.UUID, now time.Time) (Wallet, LedgerEntry, error) {
	if !initial.IsPositive() {
		return Wallet{}, LedgerEntry{}, ErrInvalidAmount
	}
	if openingTransactionID == uuid.Nil {
		return Wallet{}, LedgerEntry{}, ErrInvalidID
	}
	w, err := Open(id, playerID, initial.Currency(), now)
	if err != nil {
		return Wallet{}, LedgerEntry{}, err
	}
	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID: uuid.New(), WalletID: id, TransactionID: openingTransactionID, Direction: DirectionCredit,
		Amount: initial, BalanceBefore: w.balance, BalanceAfter: initial, CreatedAt: now,
	})
	if err != nil {
		return Wallet{}, LedgerEntry{}, err
	}
	w.balance, w.lastTransactionID = initial, openingTransactionID
	return w, entry, nil
}

// Rehydrate restores a stored wallet. It applies no movement and emits nothing; it
// only refuses what the database could never hold.
func Rehydrate(s State) (Wallet, error) {
	switch {
	case s.ID == uuid.Nil || s.PlayerID == uuid.Nil:
		return Wallet{}, ErrInvalidID
	case !s.Balance.IsValid() || s.Balance.IsNegative():
		return Wallet{}, ErrInvalidBalance
	case s.Version < 1:
		return Wallet{}, ErrInvalidVersion
	case s.Version > 1 && s.LastTransactionID == uuid.Nil:
		return Wallet{}, fmt.Errorf("%w: a wallet that moved must name its last transaction", ErrInvalidVersion)
	case s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt):
		return Wallet{}, ErrInvalidTime
	}
	return Wallet{
		id: s.ID, playerID: s.PlayerID, balance: s.Balance, version: s.Version,
		lastTransactionID: s.LastTransactionID, createdAt: s.CreatedAt.UTC(), updatedAt: s.UpdatedAt.UTC(),
	}, nil
}

// Debit takes the amount out. A debit beyond the balance is ErrInsufficientFunds.
func (w Wallet) Debit(m Movement) (Wallet, LedgerEntry, error) {
	return w.move(m, DirectionDebit)
}

// Credit puts the amount in.
func (w Wallet) Credit(m Movement) (Wallet, LedgerEntry, error) {
	return w.move(m, DirectionCredit)
}

func (w Wallet) move(m Movement, direction Direction) (Wallet, LedgerEntry, error) {
	if !w.IsValid() {
		return Wallet{}, LedgerEntry{}, ErrUninitialized
	}
	if m.EntryID == uuid.Nil || m.TransactionID == uuid.Nil {
		return Wallet{}, LedgerEntry{}, ErrInvalidID
	}
	if m.At.IsZero() {
		return Wallet{}, LedgerEntry{}, ErrInvalidTime
	}
	if !m.Amount.IsPositive() {
		return Wallet{}, LedgerEntry{}, ErrInvalidAmount
	}
	if w.version == math.MaxInt64 {
		return Wallet{}, LedgerEntry{}, fmt.Errorf("%w: the version cannot grow", ErrInvalidVersion)
	}

	var after money.Money
	var err error
	if direction == DirectionCredit {
		after, err = w.balance.Add(m.Amount)
	} else {
		after, err = w.balance.Sub(m.Amount)
	}
	if err != nil {
		return Wallet{}, LedgerEntry{}, err
	}
	if after.IsNegative() {
		return Wallet{}, LedgerEntry{}, ErrInsufficientFunds
	}
	entry, err := NewLedgerEntry(LedgerEntryParams{
		ID: m.EntryID, WalletID: w.id, TransactionID: m.TransactionID, Direction: direction,
		Amount: m.Amount, BalanceBefore: w.balance, BalanceAfter: after, CreatedAt: m.At,
	})
	if err != nil {
		return Wallet{}, LedgerEntry{}, err
	}

	next := w
	next.balance, next.version, next.lastTransactionID, next.updatedAt = after, w.version+1, m.TransactionID, m.At.UTC()
	return next, entry, nil
}

func (w Wallet) IsValid() bool            { return w.id != uuid.Nil }
func (w Wallet) ID() uuid.UUID            { return w.id }
func (w Wallet) PlayerID() uuid.UUID      { return w.playerID }
func (w Wallet) Balance() money.Money     { return w.balance }
func (w Wallet) Currency() money.Currency { return w.balance.Currency() }
func (w Wallet) Version() int64           { return w.version }
func (w Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w Wallet) UpdatedAt() time.Time     { return w.updatedAt }

// LastTransactionID is the Wager Transaction that produced the current balance;
// false for a wallet that has never moved.
func (w Wallet) LastTransactionID() (uuid.UUID, bool) {
	return w.lastTransactionID, w.lastTransactionID != uuid.Nil
}
