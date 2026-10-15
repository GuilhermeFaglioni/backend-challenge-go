package wallet

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

// Direction says whether a Ledger Entry took money out of the wallet or put it in.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// LedgerEntryParams are the fields of a Ledger Entry. The same constructor builds a
// new entry and restores a stored one: it has no side effects, only checks.
type LedgerEntryParams struct {
	ID, WalletID, TransactionID uuid.UUID
	Direction                   Direction
	Amount                      money.Money
	BalanceBefore, BalanceAfter money.Money
	CreatedAt                   time.Time
}

// LedgerEntry is the immutable record of one change of a wallet's balance. The zero
// value is invalid.
type LedgerEntry struct {
	p LedgerEntryParams
}

// NewLedgerEntry validates the fields, including the arithmetic:
// balanceAfter = balanceBefore ± amount by direction.
func NewLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.TransactionID == uuid.Nil {
		return LedgerEntry{}, ErrInvalidID
	}
	if p.Direction != DirectionDebit && p.Direction != DirectionCredit {
		return LedgerEntry{}, fmt.Errorf("%w: %q", ErrInvalidDirection, p.Direction)
	}
	if !p.Amount.IsPositive() {
		return LedgerEntry{}, ErrInvalidAmount
	}
	for _, b := range []money.Money{p.BalanceBefore, p.BalanceAfter} {
		if !b.IsValid() || b.IsNegative() {
			return LedgerEntry{}, ErrInvalidBalance
		}
	}
	if p.CreatedAt.IsZero() {
		return LedgerEntry{}, ErrInvalidTime
	}

	var expected money.Money
	var err error
	if p.Direction == DirectionCredit {
		expected, err = p.BalanceBefore.Add(p.Amount)
	} else {
		expected, err = p.BalanceBefore.Sub(p.Amount)
	}
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("wallet: ledger entry arithmetic: %w", err)
	}
	same, err := expected.Compare(p.BalanceAfter)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("wallet: ledger entry: %w", err)
	}
	if same != 0 {
		return LedgerEntry{}, ErrInconsistentEntry
	}

	p.CreatedAt = p.CreatedAt.UTC()
	return LedgerEntry{p: p}, nil
}

func (e LedgerEntry) IsValid() bool            { return e.p.ID != uuid.Nil }
func (e LedgerEntry) ID() uuid.UUID            { return e.p.ID }
func (e LedgerEntry) WalletID() uuid.UUID      { return e.p.WalletID }
func (e LedgerEntry) TransactionID() uuid.UUID { return e.p.TransactionID }
func (e LedgerEntry) Direction() Direction     { return e.p.Direction }
func (e LedgerEntry) Amount() money.Money      { return e.p.Amount }
func (e LedgerEntry) BalanceBefore() money.Money {
	return e.p.BalanceBefore
}
func (e LedgerEntry) BalanceAfter() money.Money { return e.p.BalanceAfter }
func (e LedgerEntry) CreatedAt() time.Time      { return e.p.CreatedAt }
