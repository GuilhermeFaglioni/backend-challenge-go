// Package processwager is the use case that processes an external Operation request
// on a wallet. HTTP and SQS both come here, so both get the same guarantees
// (ARCHITECTURE.md, sections 7 to 9):
//
//   - the same request sent again is answered from what was stored, with the balance
//     seen when it was first processed;
//   - the wallet is locked for the time it takes to decide and write, so operations
//     on one wallet run one at a time and the balance never goes negative;
//   - the debit, the ledger entry, the outcome and the events commit together or not
//     at all;
//   - a business refusal (a Rejection) is stored, final and replayable.
//
// Every kind of operation is processed here: the bets, wins and losses, and the Reversals
// (REFUND and ROLLBACK) and the WINs that name a Reference, which are decided against the
// Reference they resolve to. A Reference that does not exist yet is, for now, a Rejection.
package processwager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

var (
	// ErrInvalidInput marks a request the use case refuses before touching the
	// database. The domain error that explains it is wrapped.
	ErrInvalidInput = errors.New("processwager: invalid input")
	// ErrIdempotencyKeyConflict: the key was already used for a request with other
	// content.
	ErrIdempotencyKeyConflict = errors.New("processwager: the idempotency key was used with a different request")
	// ErrDuplicateExternalTransaction: the provider's external transaction id was
	// already used under another idempotency key. The operation is not applied again.
	ErrDuplicateExternalTransaction = errors.New("processwager: the external transaction id was already used with another idempotency key")
	// ErrMessageContentConflict: the inbox already holds a message with this id and
	// other content. Nothing was stored.
	ErrMessageContentConflict = errors.New("processwager: the message id was already used for a message with different content")
)

// Outcome is how a request ended, for metrics and logs.
type Outcome string

const (
	OutcomeProcessed         Outcome = "processed"
	OutcomeRejected          Outcome = "rejected"
	OutcomePending           Outcome = "pending" // accepted, waiting for its Reference
	OutcomeReplayed          Outcome = "replayed"
	OutcomeKeyConflict       Outcome = "key_conflict"
	OutcomeDuplicateExternal Outcome = "duplicate_external"
	OutcomeDuplicateMessage  Outcome = "duplicate_message" // a message delivered again: handled before, nothing done
	OutcomeMessageConflict   Outcome = "message_conflict"
	OutcomeInvalid           Outcome = "invalid"
	OutcomeUnavailable       Outcome = "unavailable" // a Transient error: nothing was stored, try again
	OutcomeError             Outcome = "error"
)

// Outcomes lists every Outcome, so a metric can start each at zero.
var Outcomes = []Outcome{
	OutcomeProcessed, OutcomeRejected, OutcomePending, OutcomeReplayed, OutcomeKeyConflict, OutcomeDuplicateExternal,
	OutcomeDuplicateMessage, OutcomeMessageConflict, OutcomeInvalid, OutcomeUnavailable, OutcomeError,
}

// Recorder measures the requests: one call per Execute.
type Recorder interface {
	Handled(kind wager.Kind, outcome Outcome, took time.Duration)
}

// Message identifies the queue message a request arrived in. The use case records it
// in the inbox in the same database transaction as everything the request changes.
type Message struct {
	Consumer string // which consumer handled it; the inbox is per consumer
	ID       string // the message id of the envelope
}

// Input is a request to process, as the adapters decode it from HTTP or SQS.
type Input struct {
	Request        wager.RequestParams
	IdempotencyKey string // as received
	CorrelationID  string
	CausationID    string   // the message or request that caused this; optional
	Message        *Message // set when the request came in a queue message
}

// Output is the stored transaction and whether it is a replay of an earlier request.
// The transaction is PROCESSED or REJECTED, with its result or its failure code. When
// the request came in a message that was handled before (MessageAlreadyHandled) nothing
// was done and the transaction is the zero value.
type Output struct {
	Transaction           wager.Transaction
	IdempotentReplay      bool
	MessageAlreadyHandled bool
}

// Deps are what the use case needs from the outside.
type Deps struct {
	UnitOfWork application.UnitOfWork
	Recorder   Recorder
	Logger     *slog.Logger
	Now        func() time.Time
	NewID      func() uuid.UUID
	// Pending says how an operation waits for a Reference that has not arrived.
	Pending application.PendingPolicy
	// AfterPending, if set, is called once the wait of an operation is committed. It is
	// where a test crashes the process to prove the wait survives it.
	AfterPending func()
}

// UseCase processes operations.
type UseCase struct {
	deps   Deps
	writer application.SettlementWriter
}

// New builds the use case.
func New(d Deps) *UseCase {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &UseCase{deps: d, writer: application.SettlementWriter{NewID: d.NewID, Now: d.Now, Policy: d.Pending}}
}

// Execute processes the request. A Transient error (errors.Is
// application.ErrTemporarilyUnavailable) means nothing was stored: the caller may try
// again with the same key.
func (u *UseCase) Execute(ctx context.Context, in Input) (Output, error) {
	started := time.Now()
	out, outcome, err := u.execute(ctx, in)
	if err == nil && outcome == OutcomePending && u.deps.AfterPending != nil {
		u.deps.AfterPending()
	}
	u.deps.Recorder.Handled(in.Request.Kind, outcome, time.Since(started))
	u.log(ctx, in, out, outcome, err)
	return out, err
}

func (u *UseCase) execute(ctx context.Context, in Input) (Output, Outcome, error) {
	request, err := wager.NewRequest(in.Request)
	if err != nil {
		return Output{}, OutcomeInvalid, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	now := u.deps.Now()
	pending, err := wager.New(wager.NewParams{
		ID: u.deps.NewID(), Request: request, IdempotencyKey: in.IdempotencyKey, CorrelationID: in.CorrelationID, At: now,
	})
	if err != nil {
		return Output{}, OutcomeInvalid, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	var out Output
	var outcome Outcome
	err = u.deps.UnitOfWork.Do(ctx, func(ctx context.Context, repos application.Repositories) error {
		return u.handle(ctx, repos, in, pending, &out, &outcome)
	})
	if err != nil {
		return Output{}, errorOutcome(err), err
	}
	return out, outcome, nil
}

// handle is one attempt for a request that may have come in a message: the message is
// recorded first (so two copies of it, handled at once, wait for each other before they
// touch any wallet) and completed last, with the same commit as everything in between.
func (u *UseCase) handle(ctx context.Context, repos application.Repositories, in Input, pending wager.Transaction, out *Output, outcome *Outcome) error {
	if in.Message != nil {
		status, err := repos.Inbox().Receive(ctx, in.Message.Consumer, in.Message.ID, messageHash(pending), u.deps.Now())
		if err != nil {
			return err
		}
		switch status {
		case application.InboxDuplicate:
			*out, *outcome = Output{MessageAlreadyHandled: true}, OutcomeDuplicateMessage
			return nil
		case application.InboxContentConflict:
			return ErrMessageContentConflict
		}
	}
	var err error
	*out, *outcome, err = u.process(ctx, repos, in, pending)
	if err != nil {
		return err
	}
	if in.Message != nil {
		return repos.Inbox().Complete(ctx, in.Message.Consumer, in.Message.ID, u.deps.Now())
	}
	return nil
}

// messageHash is what the inbox compares on a message id it has seen: the business
// content of the request and the idempotency key it came with. Transport details (the
// time it was sent, attributes) are not part of what a message says.
func messageHash(pending wager.Transaction) string {
	external := pending.External()
	sum := sha256.Sum256([]byte(string(external.PayloadHash) + "\n" + external.IdempotencyKey))
	return hex.EncodeToString(sum[:])
}

// process is one attempt, inside a unit of work. The order is the one of ADR 0002 and
// ARCHITECTURE.md section 7: look for a stored answer without any lock, then lock the
// wallet, then claim the idempotency key. Everything that waits is taken in that order
// by every operation, so they cannot deadlock one another.
func (u *UseCase) process(ctx context.Context, repos application.Repositories, in Input, pending wager.Transaction) (Output, Outcome, error) {
	transactions := repos.Transactions()

	// 1. An answer that is already stored needs no lock.
	if out, outcome, err := u.stored(ctx, transactions, pending); out.Transaction.IsValid() || err != nil {
		return out, outcome, err
	}

	// 2. The wallet: one operation at a time.
	locked, err := repos.Wallets().Lock(ctx, pending.RequestedWalletID())
	if errors.Is(err, application.ErrWalletNotFound) {
		return u.rejectUnknownWallet(ctx, repos, in, pending)
	}
	if err != nil {
		return Output{}, "", err
	}

	// 3. Claim the key. Someone may have taken it since step 1 (they would have had
	// to hold this wallet's lock, or be on another wallet with a different request).
	claimed, err := transactions.Claim(ctx, pending)
	if err != nil {
		return Output{}, "", err
	}
	if !claimed {
		return u.storedAfterLosingTheClaim(ctx, transactions, pending)
	}

	// 4. Resolve the Reference, if the operation names one. It is on this wallet, which is
	// locked, so what is read is final; and then decide and write.
	settlement, err := u.settle(ctx, transactions, pending, locked)
	if err != nil {
		return Output{}, "", err
	}
	attempt := application.Attempt{Number: 1, ExpiresAt: u.deps.Pending.ExpiresAt(pending.CreatedAt())}
	if err := u.writer.Write(ctx, repos, locked, settlement, attempt, in.CausationID); err != nil {
		return Output{}, "", err
	}

	outcome := OutcomeProcessed
	switch settlement.Transaction.Status() {
	case wager.StatusRejected:
		outcome = OutcomeRejected
	case wager.StatusPendingReference:
		outcome = OutcomePending
	}
	return Output{Transaction: settlement.Transaction}, outcome, nil
}

// settle applies the operation to the locked wallet.
func (u *UseCase) settle(ctx context.Context, transactions application.TransactionRepository, pending wager.Transaction, locked wallet.Wallet) (wager.Settlement, error) {
	external := pending.External()
	if external.ReferenceExternalTransactionID == "" {
		return pending.Settle(locked, u.deps.NewID(), u.deps.Now())
	}
	ref, err := transactions.ResolveReference(ctx, external.ProviderID, external.ReferenceExternalTransactionID)
	if err != nil {
		return wager.Settlement{}, err
	}
	return pending.SettleAgainst(ref, locked, u.deps.NewID(), u.deps.Now(), true)
}

// rejectUnknownWallet stores the Rejection WALLET_NOT_FOUND. There is no wallet to
// lock; the claim itself decides whether this is the first time the request is seen,
// and the requested wallet id is kept on the row.
func (u *UseCase) rejectUnknownWallet(ctx context.Context, repos application.Repositories, in Input, pending wager.Transaction) (Output, Outcome, error) {
	rejected, err := pending.Reject(wager.FailureWalletNotFound, u.deps.Now())
	if err != nil {
		return Output{}, "", err
	}
	claimed, err := repos.Transactions().Claim(ctx, rejected)
	if err != nil {
		return Output{}, "", err
	}
	if !claimed {
		return u.storedAfterLosingTheClaim(ctx, repos.Transactions(), pending)
	}
	if err := u.writer.Publish(ctx, repos, wager.Settlement{Transaction: rejected}, in.CausationID); err != nil {
		return Output{}, "", err
	}
	return Output{Transaction: rejected}, OutcomeRejected, nil
}

// stored looks for an answer already in the database: a replay, or a conflict. It
// returns a zero Output and no error when the request is new. It asks once: the key and
// the external id are looked up in the same statement, so they are seen in the same
// snapshot (see application.TransactionRepository.Find).
func (u *UseCase) stored(ctx context.Context, transactions application.TransactionRepository, pending wager.Transaction) (Output, Outcome, error) {
	external := pending.External()
	existing, err := transactions.Find(ctx, external.ProviderID, external.IdempotencyKey, external.ExternalTransactionID)
	if err != nil {
		return Output{}, "", err
	}
	switch {
	case existing.HasByKey:
		return replayOrConflict(existing.ByKey, pending)
	case existing.HasByExternalID:
		return Output{}, OutcomeDuplicateExternal, ErrDuplicateExternalTransaction
	}
	return Output{}, "", nil
}

// storedAfterLosingTheClaim: the claim found the key or the external id taken, so
// the answer is stored and must be found.
func (u *UseCase) storedAfterLosingTheClaim(ctx context.Context, transactions application.TransactionRepository, pending wager.Transaction) (Output, Outcome, error) {
	out, outcome, err := u.stored(ctx, transactions, pending)
	if !out.Transaction.IsValid() && err == nil {
		return Output{}, "", fmt.Errorf("the claim of transaction %s was refused, but nothing is stored under its key or external id", pending.ID())
	}
	return out, outcome, err
}

// replayOrConflict compares a stored transaction under the same key with the request.
func replayOrConflict(stored, pending wager.Transaction) (Output, Outcome, error) {
	if stored.External().PayloadHash != pending.External().PayloadHash {
		return Output{}, OutcomeKeyConflict, ErrIdempotencyKeyConflict
	}
	return Output{Transaction: stored, IdempotentReplay: true}, OutcomeReplayed, nil
}

// errorOutcome classifies an error that ended a request.
func errorOutcome(err error) Outcome {
	switch {
	case errors.Is(err, ErrIdempotencyKeyConflict):
		return OutcomeKeyConflict
	case errors.Is(err, ErrDuplicateExternalTransaction):
		return OutcomeDuplicateExternal
	case errors.Is(err, ErrMessageContentConflict):
		return OutcomeMessageConflict
	case errors.Is(err, application.ErrTemporarilyUnavailable):
		return OutcomeUnavailable
	}
	return OutcomeError
}

// log records the identifiers of the request and how it ended, never the amounts, the
// idempotency key or any payload.
func (u *UseCase) log(ctx context.Context, in Input, out Output, outcome Outcome, err error) {
	attrs := []any{
		slog.String("correlationId", in.CorrelationID),
		slog.String("providerId", in.Request.ProviderID),
		slog.String("kind", string(in.Request.Kind)),
		slog.String("walletId", in.Request.WalletID.String()),
		slog.String("outcome", string(outcome)),
	}
	if in.Message != nil {
		attrs = append(attrs, slog.String("messageId", in.Message.ID))
	}
	if out.Transaction.IsValid() {
		attrs = append(attrs,
			slog.String("transactionId", out.Transaction.ID().String()),
			slog.String("status", string(out.Transaction.Status())),
			slog.Bool("idempotentReplay", out.IdempotentReplay),
		)
		if code := out.Transaction.FailureCode(); code != "" {
			attrs = append(attrs, slog.String("failureCode", string(code)))
		}
	}
	switch outcome {
	case OutcomeProcessed, OutcomeRejected, OutcomePending, OutcomeReplayed, OutcomeDuplicateMessage:
		u.deps.Logger.InfoContext(ctx, "wager request handled", attrs...)
	case OutcomeKeyConflict, OutcomeDuplicateExternal, OutcomeMessageConflict, OutcomeInvalid:
		u.deps.Logger.WarnContext(ctx, "wager request refused", append(attrs, slog.String("reason", outcomeReason(err)))...)
	case OutcomeUnavailable:
		u.deps.Logger.WarnContext(ctx, "wager request not processed: a dependency is unavailable", append(attrs, slog.Any("error", err))...)
	default:
		u.deps.Logger.ErrorContext(ctx, "wager request failed", append(attrs, slog.Any("error", err))...)
	}
}

// outcomeReason is the short reason of a refusal, without the wrapped detail.
func outcomeReason(err error) string {
	switch {
	case errors.Is(err, ErrIdempotencyKeyConflict):
		return "idempotency key conflict"
	case errors.Is(err, ErrDuplicateExternalTransaction):
		return "duplicate external transaction"
	case errors.Is(err, ErrMessageContentConflict):
		return "message content conflict"
	case errors.Is(err, ErrInvalidInput):
		return "invalid input"
	}
	return "refused"
}
