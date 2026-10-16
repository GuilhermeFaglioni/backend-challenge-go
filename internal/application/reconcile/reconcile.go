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

// Mode is how much of the ledger a reconciliation reads. The zero value is Full: the
// check the contract describes, the balance rebuilt from every entry.
type Mode int

const (
	// Full reads the whole ledger, ignoring checkpoints for the balance, and checks the
	// last checkpoint against what the ledger adds up to through its seq.
	Full Mode = iota
	// Incremental continues from the wallet's last checkpoint: it reads only the entries
	// recorded after it. Cheaper, and it trusts the checkpoint.
	Incremental
)

func (m Mode) String() string {
	if m == Incremental {
		return "incremental"
	}
	return "full"
}

// Result is what the check found.
type Result struct {
	WalletID       uuid.UUID
	Mode           Mode
	Stored         money.Money // the balance the wallet stores
	Calculated     money.Money // credits minus debits of its ledger, the opening included
	Difference     money.Money // stored minus calculated; negative when the ledger explains more than is stored
	Consistent     bool
	CheckedEntries int64 // every entry the calculated balance accounts for, checkpoint included
	ReadEntries    int64 // the entries this check read; fewer than checked when it continued from a checkpoint
	// CheckpointMismatch: a full check found that the last checkpoint does not agree with
	// the ledger it summarises. The books are then not consistent, whatever the balance.
	CheckpointMismatch bool
	// CheckpointChecked: a full check had a checkpoint to compare with the ledger.
	CheckpointChecked bool
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
// A check that finds the books consistent records a checkpoint through the last entry it
// accounted for, so that the next incremental check reads only what came after.
func (u *UseCase) Execute(ctx context.Context, walletID uuid.UUID, mode Mode) (Result, error) {
	audit, err := u.deps.Auditor.Audit(ctx, walletID, mode == Full)
	if err != nil {
		return Result{}, err
	}

	stored, err := money.FromMinor(audit.Stored, audit.Currency)
	if err != nil {
		return Result{}, fmt.Errorf("reconcile: %w", err)
	}
	credits, debits, checked := audit.Credits, audit.Debits, audit.Entries
	checkpointed := int64(0) // the seq the books were already checkpointed through
	if audit.Base != nil {
		credits = new(big.Int).Add(credits, audit.Base.Credits)
		debits = new(big.Int).Add(debits, audit.Base.Debits)
		checked += audit.Base.Entries
		checkpointed = audit.Base.ThroughSeq
	}
	mismatch := false
	if audit.Latest != nil {
		checkpointed = audit.Latest.ThroughSeq
		mismatch = audit.Prefix == nil || !sameSums(audit.Latest.LedgerSums, *audit.Prefix)
	}
	net := new(big.Int).Sub(credits, debits)
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
		WalletID: walletID, Mode: mode, Stored: stored, Calculated: calculated, Difference: difference,
		Consistent: difference.IsZero() && !mismatch, CheckedEntries: checked, ReadEntries: audit.Entries,
		CheckpointMismatch: mismatch, CheckpointChecked: audit.Latest != nil,
	}
	if mismatch {
		u.deps.Logger.ErrorContext(ctx, "reconciliation found a checkpoint that does not match the ledger",
			slog.String("walletId", walletID.String()),
			slog.Int64("throughSeq", audit.Latest.ThroughSeq),
		)
	}
	if !difference.IsZero() {
		// Money in an ERROR line is deliberate here: the difference is the finding.
		u.deps.Logger.ErrorContext(ctx, "reconciliation found a divergence",
			slog.String("walletId", walletID.String()),
			slog.String("difference", difference.String()),
			slog.String("currency", string(audit.Currency)),
			slog.Int64("checkedEntries", checked),
		)
	}
	if result.Consistent && audit.LastSeq > checkpointed {
		// The result stands whether or not the checkpoint is recorded: without it, the
		// next check only reads more.
		if _, err := u.deps.Auditor.RecordCheckpoint(ctx, walletID, audit.LastSeq); err != nil {
			u.deps.Logger.WarnContext(ctx, "could not record the reconciliation checkpoint",
				slog.String("walletId", walletID.String()), slog.Int64("throughSeq", audit.LastSeq), slog.Any("error", err))
		}
	}
	u.deps.Recorder.Reconciled(result)
	return result, nil
}

func sameSums(a, b application.LedgerSums) bool {
	return a.Entries == b.Entries && a.Credits.Cmp(b.Credits) == 0 && a.Debits.Cmp(b.Debits) == 0
}
