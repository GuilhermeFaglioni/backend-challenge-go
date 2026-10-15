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
	// Lock reads the wallet and holds a lock on it until the database transaction
	// ends, so that operations on one wallet run one at a time while other wallets
	// go on in parallel (ADR 0002). ErrWalletNotFound if there is none.
	Lock(ctx context.Context, id uuid.UUID) (wallet.Wallet, error)
	// Update stores the wallet after a movement, but only if it is still at the
	// version of before: anything else that changed it is ErrConcurrentUpdate.
	Update(ctx context.Context, before, after wallet.Wallet) error
}

type TransactionRepository interface {
	// InsertOpening stores an internal opening in its final state.
	InsertOpening(ctx context.Context, tx wager.Transaction) error
	// FindByKey reads the provider's transaction with that idempotency key. Keys are
	// scoped by provider: one provider never sees another's.
	FindByKey(ctx context.Context, providerID, idempotencyKey string) (wager.Transaction, bool, error)
	// FindByExternalID reads the provider's transaction with that external id.
	FindByExternalID(ctx context.Context, providerID, externalTransactionID string) (wager.Transaction, bool, error)
	// Claim stores the transaction, taking its idempotency key and its external id,
	// unless either is already taken: then it stores nothing and returns false. It
	// waits for a concurrent claim of the same key to end, so the answer is final.
	Claim(ctx context.Context, tx wager.Transaction) (claimed bool, err error)
	// Finish records the outcome of a claimed transaction: its status, failure code
	// and result.
	Finish(ctx context.Context, tx wager.Transaction) error
}

type LedgerRepository interface {
	Append(ctx context.Context, entry wallet.LedgerEntry) error
}

type OutboxRepository interface {
	// Add stores the event to be published, in the same database transaction as
	// the change that caused it.
	Add(ctx context.Context, e event.Event) error
}

// WalletReader reads wallets outside any unit of work: a single consistent read.
type WalletReader interface {
	// ByID returns the wallet, or ErrWalletNotFound.
	ByID(ctx context.Context, id uuid.UUID) (wallet.Wallet, error)
}

// ErrWalletNotFound: there is no wallet with that id.
var ErrWalletNotFound = errors.New("wallet not found")

// ErrConcurrentUpdate: the wallet changed under a unit of work that held no lock on
// it, or whose lock did not protect it. The unit of work is retried.
var ErrConcurrentUpdate = errors.New("the wallet was changed concurrently")

// ErrTemporarilyUnavailable marks an error that is the infrastructure's, not the
// request's, and that trying again later may fix: the database cannot be reached,
// a connection dropped, the caller's context ended, or a serialization failure or
// deadlock persisted through every retry. Adapters wrap such errors with it.
var ErrTemporarilyUnavailable = errors.New("a dependency is temporarily unavailable")

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
