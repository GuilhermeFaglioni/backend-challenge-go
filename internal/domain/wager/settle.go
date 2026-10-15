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

// Settle applies the transaction to its wallet. A business rule that refuses the
// operation is not an error: it is the outcome, a Settlement whose transaction is
// REJECTED and whose wallet and entry are untouched. An error means the call itself
// was wrong (another wallet, a transaction that already ended, a kind not supported).
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
	if !t.IsValid() || !w.IsValid() {
		return Settlement{}, ErrUninitialized
	}
	if t.kind != KindBet && t.kind != KindWin && t.kind != KindLoss {
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

	movement := wallet.Movement{EntryID: entryID, TransactionID: t.id, Amount: t.amount, At: at}
	next, entry := w, wallet.LedgerEntry{}
	var err error
	switch t.kind {
	case KindBet:
		next, entry, err = w.Debit(movement)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return reject(FailureInsufficientFunds)
		}
	case KindWin:
		next, entry, err = w.Credit(movement)
		if errors.Is(err, money.ErrOverflow) {
			return reject(FailureBalanceLimitExceeded)
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
