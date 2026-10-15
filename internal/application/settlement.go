package application

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// SettlementWriter writes what the domain decided about an operation: the movement, the
// outcome and the events, in the unit of work the caller opened. Both ways an operation
// is decided use it, when it arrives (processwager) and when it is taken up again after
// waiting for its Reference (resolvepending), so they cannot drift apart.
type SettlementWriter struct {
	NewID  func() uuid.UUID
	Now    func() time.Time
	Policy PendingPolicy
}

// Attempt says which try of an operation this is and when its wait ends.
type Attempt struct {
	Number    int       // 1 when the operation arrives, then one more for each try of the worker
	ExpiresAt time.Time // when the operation stops waiting for its Reference
}

// Write stores a settlement. A decided operation moves the wallet if it has an entry,
// records its outcome and emits its events. An operation left waiting only has its wait
// recorded: the first time with WagerTransactionPendingReference, later tries silently.
// causationID is the message or request that caused the events.
func (w SettlementWriter) Write(ctx context.Context, repos Repositories, locked wallet.Wallet, s wager.Settlement, attempt Attempt, causationID string) error {
	tx := s.Transaction
	if s.Waiting() {
		now := w.Now()
		if err := repos.Transactions().Await(ctx, tx, PendingSchedule{
			Attempts:      attempt.Number,
			NextAttemptAt: w.Policy.NextAttempt(now, attempt.Number, attempt.ExpiresAt),
			ExpiresAt:     attempt.ExpiresAt,
		}); err != nil {
			return err
		}
		if attempt.Number > 1 {
			return nil
		}
		external := tx.External()
		pending, err := event.NewWagerTransactionPendingReference(w.meta(tx, causationID), event.WagerTransactionPendingReferenceData{
			TransactionID: tx.ID(), WalletID: tx.WalletID(), PlayerID: tx.PlayerID(), Kind: tx.Kind(),
			ProviderID: external.ProviderID, ExternalTransactionID: external.ExternalTransactionID,
			RoundID: external.RoundID, GameID: external.GameID, ReferenceExternalTransactionID: external.ReferenceExternalTransactionID,
			Money: tx.Amount(), ExpiresAt: attempt.ExpiresAt,
		})
		if err != nil {
			return err
		}
		return repos.Outbox().Add(ctx, pending)
	}

	if s.Entry.IsValid() {
		if err := repos.Wallets().Update(ctx, locked, s.Wallet); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, s.Entry); err != nil {
			return err
		}
	}
	if err := repos.Transactions().Finish(ctx, tx); err != nil {
		return err
	}
	return w.Publish(ctx, repos, s, causationID)
}

// Publish writes the events of a decided operation to the outbox.
func (w SettlementWriter) Publish(ctx context.Context, repos Repositories, s wager.Settlement, causationID string) error {
	tx := s.Transaction
	external := tx.External()

	var events []event.Event
	if tx.Status() == wager.StatusRejected {
		rejected, err := event.NewWagerTransactionRejected(w.meta(tx, causationID), event.WagerTransactionRejectedData{
			TransactionID: tx.ID(), WalletID: tx.RequestedWalletID(), PlayerID: tx.PlayerID(), Kind: tx.Kind(),
			ProviderID: external.ProviderID, ExternalTransactionID: external.ExternalTransactionID,
			RoundID: external.RoundID, GameID: external.GameID, Money: tx.Amount(), FailureCode: tx.FailureCode(),
			ReferenceExternalTransactionID: external.ReferenceExternalTransactionID,
		})
		if err != nil {
			return err
		}
		events = append(events, rejected)
	} else {
		result, _ := tx.Result()
		processed, err := event.NewWagerTransactionProcessed(w.meta(tx, causationID), event.WagerTransactionProcessedData{
			TransactionID: tx.ID(), WalletID: tx.WalletID(), PlayerID: tx.PlayerID(), Kind: tx.Kind(), Origin: tx.Origin(),
			ProviderID: external.ProviderID, ExternalTransactionID: external.ExternalTransactionID,
			RoundID: external.RoundID, GameID: external.GameID, ReferenceExternalTransactionID: external.ReferenceExternalTransactionID,
			Money: tx.Amount(), Balance: result.Balance, WalletVersion: result.WalletVersion,
		})
		if err != nil {
			return err
		}
		events = append(events, processed)
	}
	if s.Entry.IsValid() {
		changed, err := event.NewWalletBalanceChanged(w.meta(tx, causationID), event.WalletBalanceChangedData{
			WalletID: s.Wallet.ID(), TransactionID: tx.ID(), Direction: s.Entry.Direction(), Money: s.Entry.Amount(),
			BalanceBefore: s.Entry.BalanceBefore(), BalanceAfter: s.Entry.BalanceAfter(), WalletVersion: s.Wallet.Version(),
		})
		if err != nil {
			return err
		}
		events = append(events, changed)
	}
	for _, e := range events {
		if err := repos.Outbox().Add(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (w SettlementWriter) meta(tx wager.Transaction, causationID string) event.Meta {
	return event.Meta{EventID: w.NewID(), CorrelationID: tx.CorrelationID(), CausationID: causationID, OccurredAt: tx.UpdatedAt()}
}
