// Package application holds the ports the use cases depend on (the unit of work and
// the repositories). The use cases themselves live in subpackages (openwallet,
// later processwager, resolvepending and reconcile). It imports the domain only;
// adapters implement the ports.
package application

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// UnitOfWork delimits a database transaction. The use case decides the scope;
// repositories never begin or end a transaction on their own, so the wallet, the
// Wager Transaction, the ledger and the outbox are committed atomically. Do rolls
// back on any error and on panic, retries the function a bounded number of times
// on serialization failure and deadlock, and stops when the context is cancelled.
// The function may therefore run more than once and must have no side effects
// outside the transaction.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context, repos Repositories) error) error
}

// Repositories are bound to the transaction of one Do.
type Repositories interface {
	Wallets() WalletRepository
	Transactions() TransactionRepository
	Ledger() LedgerRepository
	Outbox() OutboxRepository
}

type WalletRepository interface {
	// Insert stores a new wallet. If the player already has a wallet in that
	// currency it stores nothing and returns *WalletAlreadyExistsError.
	Insert(ctx context.Context, w wallet.Wallet) error
}

type TransactionRepository interface {
	// InsertOpening stores an internal opening in its final state.
	InsertOpening(ctx context.Context, tx wager.Transaction) error
}

type LedgerRepository interface {
	Append(ctx context.Context, entry wallet.LedgerEntry) error
}

type OutboxRepository interface {
	// Add stores the event to be published, in the same database transaction as
	// the change that caused it.
	Add(ctx context.Context, e event.Event) error
}

// ErrWalletAlreadyExists is matched by errors.Is for *WalletAlreadyExistsError.
var ErrWalletAlreadyExists = errors.New("a wallet already exists for the player and currency")

// WalletAlreadyExistsError carries the wallet that is already there, so a client
// that retried after a timeout can confirm the result.
type WalletAlreadyExistsError struct {
	WalletID uuid.UUID
}

func (e *WalletAlreadyExistsError) Error() string {
	return ErrWalletAlreadyExists.Error() + ": " + e.WalletID.String()
}

func (e *WalletAlreadyExistsError) Is(target error) bool { return target == ErrWalletAlreadyExists }
