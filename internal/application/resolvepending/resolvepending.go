// Package resolvepending takes up again an operation that was accepted before its
// Reference arrived (ARCHITECTURE.md, section 10). Each try runs in a database
// transaction of its own: the wallet is locked first, in the same order as for an
// operation that arrives, then the operation is read again, because between the
// reservation and the lock another worker may have decided it.
//
// A try ends in one of four ways: the operation is applied or refused like any other
// (the Reference came, or came wrong, or the wallet's own rules refuse it), it is left
// waiting for the next try, or, when it may not wait any more, it is refused because its
// Reference never appeared or never completed. Operations that were accepted and never
// finished (PENDING, which an acceptance only leaves behind if it died half way) are
// finished the same way.
package resolvepending

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// Outcome is how a try ended, for metrics and logs.
type Outcome string

const (
	OutcomeProcessed Outcome = "processed" // the Reference was there and the operation was applied
	OutcomeRejected  Outcome = "rejected"  // a rule refused it, whatever the Reference
	OutcomeWaiting   Outcome = "waiting"   // still no Reference: tried again later
	OutcomeExpired   Outcome = "expired"   // it may not wait any more, and was refused for it
	OutcomeDiscarded Outcome = "discarded" // already decided by someone else: nothing to do
)

// Outcomes lists every Outcome, so a metric can start each at zero.
var Outcomes = []Outcome{OutcomeProcessed, OutcomeRejected, OutcomeWaiting, OutcomeExpired, OutcomeDiscarded}

// Recorder is told every try that reached an outcome.
type Recorder interface {
	Resolved(Outcome)
}

// Result is what a try did.
type Result struct {
	Outcome       Outcome
	TransactionID uuid.UUID
	Attempts      int // tries so far, this one included; 0 for a discarded one
}

// Deps are what the use case needs from the outside.
type Deps struct {
	UnitOfWork application.UnitOfWork
	Recorder   Recorder
	Logger     *slog.Logger
	Now        func() time.Time
	NewID      func() uuid.UUID
	Policy     application.PendingPolicy
}

// UseCase resolves waiting operations.
type UseCase struct {
	deps   Deps
	writer application.SettlementWriter
}

func New(d Deps) *UseCase {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &UseCase{deps: d, writer: application.SettlementWriter{NewID: d.NewID, Now: d.Now, Policy: d.Policy}}
}

// Execute makes one try for the operation. An error means nothing was written (the
// unit of work was rolled back) and the operation stays as it was, to be taken up again
// when its lease runs out.
func (u *UseCase) Execute(ctx context.Context, item application.PendingItem) (Result, error) {
	var result Result
	err := u.deps.UnitOfWork.Do(ctx, func(ctx context.Context, repos application.Repositories) error {
		var err error
		result, err = u.try(ctx, repos, item)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	u.deps.Recorder.Resolved(result.Outcome)
	u.log(ctx, result)
	return result, nil
}

func (u *UseCase) try(ctx context.Context, repos application.Repositories, item application.PendingItem) (Result, error) {
	// The wallet first, as every operation does; then the operation, read again.
	locked, err := repos.Wallets().Lock(ctx, item.WalletID)
	if err != nil {
		return Result{}, fmt.Errorf("lock the wallet of pending transaction %s: %w", item.TransactionID, err)
	}
	pending, schedule, found, err := repos.Transactions().ReadPending(ctx, item.TransactionID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return Result{Outcome: OutcomeDiscarded, TransactionID: item.TransactionID}, nil
	}

	now := u.deps.Now()
	attempt := application.Attempt{Number: schedule.Attempts + 1, ExpiresAt: schedule.ExpiresAt}
	if attempt.ExpiresAt.IsZero() { // an acceptance that never finished has no schedule yet
		attempt.ExpiresAt = u.deps.Policy.ExpiresAt(pending.CreatedAt())
	}
	final := u.deps.Policy.Exhausted(attempt.Number, now, attempt.ExpiresAt)

	settlement, err := u.settle(ctx, repos.Transactions(), pending, locked, now, !final)
	if err != nil {
		return Result{}, err
	}
	if err := u.writer.Write(ctx, repos, locked, settlement, attempt, pending.ID().String()); err != nil {
		return Result{}, err
	}

	result := Result{TransactionID: pending.ID(), Attempts: attempt.Number}
	switch {
	case settlement.Waiting():
		result.Outcome = OutcomeWaiting
	case settlement.Transaction.Status() == wager.StatusProcessed:
		result.Outcome = OutcomeProcessed
	case final && settlement.Transaction.FailureCode() == wager.FailureReferenceNotFound,
		final && settlement.Transaction.FailureCode() == wager.FailureReferenceNotProcessed:
		result.Outcome = OutcomeExpired
	default:
		result.Outcome = OutcomeRejected
	}
	return result, nil
}

func (u *UseCase) settle(ctx context.Context, transactions application.TransactionRepository, pending wager.Transaction, locked wallet.Wallet, now time.Time, mayWait bool) (wager.Settlement, error) {
	external := pending.External()
	if external.ReferenceExternalTransactionID == "" {
		return pending.Settle(locked, u.deps.NewID(), now)
	}
	ref, err := transactions.ResolveReference(ctx, external.ProviderID, external.ReferenceExternalTransactionID)
	if err != nil {
		return wager.Settlement{}, err
	}
	return pending.SettleAgainst(ref, locked, u.deps.NewID(), now, mayWait)
}

func (u *UseCase) log(ctx context.Context, r Result) {
	attrs := []any{
		slog.String("transactionId", r.TransactionID.String()),
		slog.String("outcome", string(r.Outcome)),
		slog.Int("attempts", r.Attempts),
	}
	switch r.Outcome {
	case OutcomeWaiting, OutcomeDiscarded:
		u.deps.Logger.DebugContext(ctx, "pending operation tried", attrs...)
	default:
		u.deps.Logger.InfoContext(ctx, "pending operation resolved", attrs...)
	}
}
