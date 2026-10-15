// Package application holds the ports the use cases depend on (the unit of work and
// the repositories). The use cases themselves live in subpackages (openwallet,
// later processwager, resolvepending and reconcile). It imports the domain only;
// adapters implement the ports.
package application

import (
	"context"
	"errors"
	"math/big"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
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
	// Find reads the provider's transactions that have this idempotency key or this
	// external id, in ONE statement, so both answers come from the same snapshot of the
	// database. Two separate lookups would not: a transaction committed between them
	// could be missed by the first and found by the second, and the same request would
	// look like two different ones. Keys and ids are scoped by provider: one provider
	// never sees another's.
	Find(ctx context.Context, providerID, idempotencyKey, externalTransactionID string) (ExistingTransactions, error)
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

// ExistingTransactions is what Find found: the transaction with the idempotency key,
// and the one with the external id (the same one, when the same request was stored).
type ExistingTransactions struct {
	ByKey, ByExternalID       wager.Transaction
	HasByKey, HasByExternalID bool
}

// Scope limits which transactions a read can reach. It is part of the query, not a
// check made afterwards: a transaction outside the scope is never read, so a mistake in
// a handler cannot leak it. The zero Scope reaches nothing.
type Scope struct {
	providerID string
	all        bool
}

// AllTransactions reaches every transaction, including the internal openings. For the
// internal service.
func AllTransactions() Scope { return Scope{all: true} }

// ProviderTransactions reaches only the external transactions of that provider. An empty
// provider id reaches nothing.
func ProviderTransactions(providerID string) Scope { return Scope{providerID: providerID} }

func (s Scope) All() bool          { return s.all }
func (s Scope) ProviderID() string { return s.providerID }

// Reaches reports whether a transaction of that provider ("" for an internal one) is in
// the scope. Readers apply the same rule in their queries; this is for tests and fakes.
func (s Scope) Reaches(providerID string) bool {
	return s.all || (s.providerID != "" && s.providerID == providerID)
}

// TransactionReader reads Wager Transactions outside any unit of work.
type TransactionReader interface {
	// ByID returns the transaction with that id if it is in the scope, else
	// ErrTransactionNotFound: a transaction that exists but is out of scope is
	// indistinguishable from one that does not exist.
	ByID(ctx context.Context, id uuid.UUID, scope Scope) (wager.Transaction, error)
	// ByExternalID returns the provider's transaction with that external id, or
	// ErrTransactionNotFound.
	ByExternalID(ctx context.Context, providerID, externalTransactionID string) (wager.Transaction, error)
}

// ErrTransactionNotFound: there is no such transaction among those the caller may read.
var ErrTransactionNotFound = errors.New("transaction not found")

// LedgerRecord is a ledger entry with its position in the wallet's sequence.
type LedgerRecord struct {
	Seq   int64
	Entry wallet.LedgerEntry
}

// LedgerReader reads a wallet's ledger by pages.
type LedgerReader interface {
	// List returns up to limit entries of the wallet's ledger whose position is after
	// the given one (0 for the beginning), oldest first. The position is the database's
	// sequence, which for one wallet is also the order of commit: an entry that is
	// written later is always after, so a reader that has seen up to a position never
	// finds an earlier one appearing.
	List(ctx context.Context, walletID uuid.UUID, after int64, limit int) ([]LedgerRecord, error)
}

// LedgerAudit is what a wallet's books look like at one instant: the balance it stores
// and the sums of its ledger. Credits and debits are kept apart so that neither can
// overflow what a single signed sum could.
type LedgerAudit struct {
	Currency money.Currency
	Stored   int64 // balance_minor
	Credits  *big.Int
	Debits   *big.Int
	Entries  int64
}

// LedgerAuditor reads a wallet's books for reconciliation.
type LedgerAuditor interface {
	// Audit reads the stored balance and the ledger in ONE consistent snapshot, read
	// only, so that writes going on at the same time cannot make them disagree. It
	// changes nothing. ErrWalletNotFound if there is no such wallet.
	Audit(ctx context.Context, walletID uuid.UUID) (LedgerAudit, error)
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
