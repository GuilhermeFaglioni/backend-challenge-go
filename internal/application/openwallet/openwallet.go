// Package openwallet is the use case that opens a wallet for a player. With a
// positive initial balance it also records the internal opening, the credit that
// funds the wallet and the events about both, all in one database transaction
// (ARCHITECTURE.md, section 9).
package openwallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// ErrInvalidInput marks input the use case refuses before touching the database.
// The domain error that explains it is wrapped.
var ErrInvalidInput = errors.New("openwallet: invalid input")

// Outcome is how an attempt to open a wallet ended, for metrics.
type Outcome string

const (
	OutcomeCreated  Outcome = "created"
	OutcomeConflict Outcome = "conflict" // the player already has a wallet in that currency
	OutcomeInvalid  Outcome = "invalid"
	OutcomeError    Outcome = "error"
)

// Outcomes lists every Outcome, so a metric can start each at zero.
var Outcomes = []Outcome{OutcomeCreated, OutcomeConflict, OutcomeInvalid, OutcomeError}

// Recorder counts the outcomes.
type Recorder interface {
	Opened(Outcome)
}

// Input is a request to open a wallet. A zero InitialBalance is allowed (the wallet
// starts empty); a negative one is not.
type Input struct {
	PlayerID       uuid.UUID
	InitialBalance money.Money
	CorrelationID  string
}

// Deps are what the use case needs from the outside.
type Deps struct {
	UnitOfWork application.UnitOfWork
	Recorder   Recorder
	Now        func() time.Time
	NewID      func() uuid.UUID
}

// UseCase opens wallets.
type UseCase struct {
	deps Deps
}

// New builds the use case.
func New(d Deps) *UseCase { return &UseCase{deps: d} }

// Execute opens the wallet and returns it. A wallet that already exists for the
// player and currency is *application.WalletAlreadyExistsError, carrying its id.
func (u *UseCase) Execute(ctx context.Context, in Input) (wallet.Wallet, error) {
	plan, err := u.plan(in)
	if err != nil {
		u.deps.Recorder.Opened(OutcomeInvalid)
		return wallet.Wallet{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	err = u.deps.UnitOfWork.Do(ctx, plan.persist)
	switch {
	case err == nil:
		u.deps.Recorder.Opened(OutcomeCreated)
		return plan.wallet, nil
	case errors.Is(err, application.ErrWalletAlreadyExists):
		u.deps.Recorder.Opened(OutcomeConflict)
	default:
		u.deps.Recorder.Opened(OutcomeError)
	}
	return wallet.Wallet{}, err
}

// opening is everything the use case decided before the transaction begins. Building
// it first keeps the transaction short, and means a retried transaction writes the
// same ids.
type opening struct {
	wallet wallet.Wallet
	// The remaining fields are set only for a positive initial balance.
	transaction wager.Transaction
	entry       wallet.LedgerEntry
	events      []event.Event
	funded      bool
}

func (u *UseCase) plan(in Input) (*opening, error) {
	now := u.deps.Now()
	// The domain constructors refuse a missing player and a balance that is
	// uninitialized or negative. The correlation id is checked here because an empty
	// wallet creates no transaction or event that would.
	if in.CorrelationID == "" {
		return nil, wager.ErrInvalidCorrelation
	}
	walletID := u.deps.NewID()

	if in.InitialBalance.IsZero() {
		w, err := wallet.Open(walletID, in.PlayerID, in.InitialBalance.Currency(), now)
		if err != nil {
			return nil, err
		}
		return &opening{wallet: w}, nil
	}

	transactionID := u.deps.NewID()
	w, entry, err := wallet.OpenWithBalance(walletID, in.PlayerID, in.InitialBalance, transactionID, now)
	if err != nil {
		return nil, err
	}
	pending, err := wager.NewOpening(wager.OpeningParams{
		ID: transactionID, WalletID: walletID, PlayerID: in.PlayerID,
		Amount: in.InitialBalance, CorrelationID: in.CorrelationID, At: now,
	})
	if err != nil {
		return nil, err
	}
	processed, err := pending.Process(wager.Result{Balance: w.Balance(), WalletVersion: w.Version()}, now)
	if err != nil {
		return nil, err
	}

	meta := func() event.Meta {
		return event.Meta{EventID: u.deps.NewID(), CorrelationID: in.CorrelationID, OccurredAt: now}
	}
	wasProcessed, err := event.NewWagerTransactionProcessed(meta(), event.WagerTransactionProcessedData{
		TransactionID: processed.ID(), WalletID: walletID, PlayerID: in.PlayerID,
		Kind: processed.Kind(), Origin: processed.Origin(),
		Money: processed.Amount(), Balance: w.Balance(), WalletVersion: w.Version(),
	})
	if err != nil {
		return nil, err
	}
	balanceChanged, err := event.NewWalletBalanceChanged(meta(), event.WalletBalanceChangedData{
		WalletID: walletID, TransactionID: processed.ID(), Direction: entry.Direction(),
		Money: entry.Amount(), BalanceBefore: entry.BalanceBefore(), BalanceAfter: entry.BalanceAfter(),
		WalletVersion: w.Version(),
	})
	if err != nil {
		return nil, err
	}
	return &opening{
		wallet: w, transaction: processed, entry: entry, funded: true,
		events: []event.Event{wasProcessed, balanceChanged},
	}, nil
}

// persist writes the opening inside a unit of work. The wallet goes first because
// the transaction and the entry point at it.
func (o *opening) persist(ctx context.Context, repos application.Repositories) error {
	if err := repos.Wallets().Insert(ctx, o.wallet); err != nil {
		return err
	}
	if !o.funded {
		return nil
	}
	if err := repos.Transactions().InsertOpening(ctx, o.transaction); err != nil {
		return err
	}
	if err := repos.Ledger().Append(ctx, o.entry); err != nil {
		return err
	}
	for _, e := range o.events {
		if err := repos.Outbox().Add(ctx, e); err != nil {
			return err
		}
	}
	return nil
}
