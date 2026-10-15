package wager

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// Settlement is what applying a transaction to a wallet produced: the transaction in
// its final state, the wallet afterwards (the same wallet if nothing moved) and the
// Ledger Entry of the movement (invalid if nothing moved).
type Settlement struct {
	Transaction Transaction
	Wallet      wallet.Wallet
	Entry       wallet.LedgerEntry
}

// Reference is what the operation's Reference resolved to, or that it resolved to
// nothing: the zero value is "not found".
type Reference struct {
	Found       bool
	Transaction Transaction // the referenced Wager Transaction, whatever its state
	// Reversed: a processed Reversal already points at it. Only a Reversal looks at it.
	Reversed bool
}

// Settle applies an operation that names no Reference to its wallet. A business rule
// that refuses the operation is not an error: it is the outcome, a Settlement whose
// transaction is REJECTED and whose wallet and entry are untouched. An error means the
// call itself was wrong (another wallet, a transaction that already ended, a kind that
// cannot be settled, an operation that does need its Reference).
//
// The rules are checked in this order: the wallet belongs to the player
// (PLAYER_MISMATCH) and it is in the operation's currency (CURRENCY_MISMATCH). Then
// the kind decides:
//
//   - BET debits, and is refused for lack of funds (INSUFFICIENT_FUNDS);
//   - WIN credits, and is refused if the balance could not hold the prize
//     (BALANCE_LIMIT_EXCEEDED);
//   - LOSS records a result and moves nothing: no Ledger Entry, and the wallet comes
//     back the very same, so its version does not change.
func (t Transaction) Settle(w wallet.Wallet, entryID uuid.UUID, at time.Time) (Settlement, error) {
	if t.external.ReferenceExternalTransactionID != "" || t.kind == KindRefund || t.kind == KindRollback {
		return Settlement{}, ErrReferenceUnresolved
	}
	return t.settle(w, Reference{}, entryID, at)
}

// SettleAgainst applies an operation that names a Reference: a WIN that points at its
// BET, or a Reversal. On top of the rules of Settle, the Reference decides, in this
// order (ARCHITECTURE.md, section 10):
//
//	not found, or still waiting      REFERENCE_NOT_FOUND (until references can be waited for)
//	ended without success            REFERENCE_NOT_PROCESSED
//	not a kind this can act on       REFERENCE_KIND_INVALID
//	other player, wallet, currency
//	or round                         REFERENCE_MISMATCH
//	other amount (a Reversal)        REFERENCE_AMOUNT_MISMATCH
//	already has a Reversal           REFERENCE_ALREADY_REVERSED
//
// Then the money moves: a REFUND credits what the BET took; a ROLLBACK does the contrary
// of what the Reference did (it credits for a BET, it debits for a WIN or a REFUND). A
// Reversal that would take the balance below zero is REVERSAL_INSUFFICIENT_FUNDS, told
// apart from a BET's. Once the Reference is found, its id is kept on the transaction,
// whatever the outcome.
func (t Transaction) SettleAgainst(ref Reference, w wallet.Wallet, entryID uuid.UUID, at time.Time) (Settlement, error) {
	if t.external.ReferenceExternalTransactionID == "" {
		return Settlement{}, ErrNoReference
	}
	if ref.Found && !ref.Transaction.IsValid() {
		return Settlement{}, ErrUninitialized
	}
	return t.settle(w, ref, entryID, at)
}

func (t Transaction) settle(w wallet.Wallet, ref Reference, entryID uuid.UUID, at time.Time) (Settlement, error) {
	if !t.IsValid() || !w.IsValid() {
		return Settlement{}, ErrUninitialized
	}
	switch t.kind {
	case KindBet, KindWin, KindLoss, KindRefund, KindRollback:
	default:
		return Settlement{}, fmt.Errorf("%w: %s", ErrKindNotSupported, t.kind)
	}
	if w.ID() != t.walletID {
		return Settlement{}, ErrWalletMismatch
	}

	reject := func(code FailureCode) (Settlement, error) {
		rejected, err := t.Reject(code, at)
		if err != nil {
			return Settlement{}, err
		}
		return Settlement{Transaction: rejected, Wallet: w}, nil
	}
	switch {
	case w.PlayerID() != t.playerID:
		return reject(FailurePlayerMismatch)
	case w.Currency() != t.amount.Currency():
		return reject(FailureCurrencyMismatch)
	}

	// The direction of the movement. A Reversal takes it from what it reverses.
	credit := t.kind == KindWin || t.kind == KindRefund
	if t.external.ReferenceExternalTransactionID != "" {
		if code, refused := t.judge(ref); refused {
			if ref.Found {
				t = t.resolvedTo(ref.Transaction)
			}
			return reject(code)
		}
		t = t.resolvedTo(ref.Transaction)
		if t.kind == KindRollback {
			credit = ref.Transaction.kind == KindBet
		}
	}

	movement := wallet.Movement{EntryID: entryID, TransactionID: t.id, Amount: t.amount, At: at}
	next, entry := w, wallet.LedgerEntry{}
	var err error
	switch {
	case t.kind == KindLoss:
	case credit:
		next, entry, err = w.Credit(movement)
		if errors.Is(err, money.ErrOverflow) {
			return reject(FailureBalanceLimitExceeded)
		}
	default:
		next, entry, err = w.Debit(movement)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			if t.isReversal() {
				return reject(FailureReversalInsufficientFunds)
			}
			return reject(FailureInsufficientFunds)
		}
	}
	if err != nil {
		return Settlement{}, err
	}
	processed, err := t.Process(Result{Balance: next.Balance(), WalletVersion: next.Version()}, at)
	if err != nil {
		return Settlement{}, err
	}
	return Settlement{Transaction: processed, Wallet: next, Entry: entry}, nil
}

func (t Transaction) isReversal() bool { return t.kind == KindRefund || t.kind == KindRollback }

// resolvedTo keeps the id of the Reference the operation resolved to.
func (t Transaction) resolvedTo(ref Transaction) Transaction {
	t.external.ReferenceTransactionID = ref.id
	return t
}

// judge applies the table of References and says which code refuses the operation,
// if one does.
func (t Transaction) judge(ref Reference) (FailureCode, bool) {
	if !ref.Found {
		return FailureReferenceNotFound, true
	}
	r := ref.Transaction
	switch r.status {
	case StatusPending, StatusPendingReference:
		return FailureReferenceNotFound, true
	case StatusRejected, StatusFailed:
		return FailureReferenceNotProcessed, true
	}

	if !t.canActOn(r.kind) {
		return FailureReferenceKindInvalid, true
	}
	if r.playerID != t.playerID || r.walletID != t.walletID || r.amount.Currency() != t.amount.Currency() ||
		r.external.ProviderID != t.external.ProviderID || r.external.RoundID != t.external.RoundID {
		return FailureReferenceMismatch, true
	}
	if t.isReversal() {
		if r.amount != t.amount {
			return FailureReferenceAmountMismatch, true
		}
		if ref.Reversed {
			return FailureReferenceAlreadyReversed, true
		}
	}
	return "", false
}

// canActOn says which kinds of Reference this operation can act on: a WIN names the BET
// it pays, a REFUND gives a BET back, a ROLLBACK undoes a BET, a WIN or a REFUND. A LOSS
// moves nothing and a ROLLBACK is not undone (the BET is not reopened).
func (t Transaction) canActOn(referenced Kind) bool {
	switch t.kind {
	case KindWin, KindRefund:
		return referenced == KindBet
	case KindRollback:
		return referenced == KindBet || referenced == KindWin || referenced == KindRefund
	}
	return false
}
