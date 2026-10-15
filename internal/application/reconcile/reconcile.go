// Package reconcile checks a wallet's books: it rebuilds the balance from the ledger and
// compares it with the balance the wallet stores (ARCHITECTURE.md, section 9). It never
// changes anything; a divergence is a finding, reported, not repaired.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

// ErrBooksOutOfRange: the ledger adds up to more than a balance can hold. Something is
// very wrong with the data, and the check says so instead of wrapping around.
var ErrBooksOutOfRange = errors.New("reconcile: the ledger adds up to an amount out of range")

// Result is what the check found.
type Result struct {
	WalletID       uuid.UUID
	Stored         money.Money // the balance the wallet stores
	Calculated     money.Money // credits minus debits of its ledger, the opening included
	Difference     money.Money // stored minus calculated; negative when the ledger explains more than is stored
	Consistent     bool
	CheckedEntries int64
}

// Recorder is told every result, to count the divergences.
type Recorder interface {
	Reconciled(Result)
}

// Deps are what the use case needs from the outside.
type Deps struct {
	Auditor  application.LedgerAuditor
	Recorder Recorder
	Logger   *slog.Logger
}

// UseCase reconciles wallets.
type UseCase struct {
	deps Deps
}

func New(d Deps) *UseCase {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &UseCase{deps: d}
}

// Execute reconciles the wallet. ErrWalletNotFound (from the auditor) if there is none.
func (u *UseCase) Execute(ctx context.Context, walletID uuid.UUID) (Result, error) {
	audit, err := u.deps.Auditor.Audit(ctx, walletID)
	if err != nil {
		return Result{}, err
	}

	stored, err := money.FromMinor(audit.Stored, audit.Currency)
	if err != nil {
		return Result{}, fmt.Errorf("reconcile: %w", err)
	}
	net := new(big.Int).Sub(audit.Credits, audit.Debits)
	if !net.IsInt64() {
		return Result{}, fmt.Errorf("%w: %s", ErrBooksOutOfRange, net)
	}
	calculated, err := money.FromMinor(net.Int64(), audit.Currency)
	if err != nil {
		return Result{}, fmt.Errorf("reconcile: %w", err)
	}
	difference, err := stored.Sub(calculated)
	if err != nil {
		if errors.Is(err, money.ErrOverflow) {
			return Result{}, fmt.Errorf("%w: stored %s minus calculated %s", ErrBooksOutOfRange, stored, calculated)
		}
		return Result{}, fmt.Errorf("reconcile: %w", err)
	}

	result := Result{
		WalletID: walletID, Stored: stored, Calculated: calculated, Difference: difference,
		Consistent: difference.IsZero(), CheckedEntries: audit.Entries,
	}
	if !result.Consistent {
		// Money in an ERROR line is deliberate here: the difference is the finding.
		u.deps.Logger.ErrorContext(ctx, "reconciliation found a divergence",
			slog.String("walletId", walletID.String()),
			slog.String("difference", difference.String()),
			slog.String("currency", string(audit.Currency)),
			slog.Int64("checkedEntries", audit.Entries),
		)
	}
	u.deps.Recorder.Reconciled(result)
	return result, nil
}
