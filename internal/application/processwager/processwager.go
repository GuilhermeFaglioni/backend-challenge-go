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
// Only the BET is processed so far; the other kinds arrive with their tickets.
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
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

var (
	// ErrInvalidInput marks a request the use case refuses before touching the
	// database. The domain error that explains it is wrapped.
	ErrInvalidInput = errors.New("processwager: invalid input")
	// ErrUnsupportedKind: a valid kind this use case does not process yet.
	ErrUnsupportedKind = errors.New("processwager: this kind of operation is not processed yet")
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
	OutcomeReplayed          Outcome = "replayed"
	OutcomeKeyConflict       Outcome = "key_conflict"
	OutcomeDuplicateExternal Outcome = "duplicate_external"
	OutcomeDuplicateMessage  Outcome = "duplicate_message" // a message delivered again: handled before, nothing done
	OutcomeMessageConflict   Outcome = "message_conflict"
	OutcomeInvalid           Outcome = "invalid"
	OutcomeUnsupported       Outcome = "unsupported"
	OutcomeUnavailable       Outcome = "unavailable" // a Transient error: nothing was stored, try again
	OutcomeError             Outcome = "error"
)

// Outcomes lists every Outcome, so a metric can start each at zero.
var Outcomes = []Outcome{
	OutcomeProcessed, OutcomeRejected, OutcomeReplayed, OutcomeKeyConflict, OutcomeDuplicateExternal,
	OutcomeDuplicateMessage, OutcomeMessageConflict, OutcomeInvalid, OutcomeUnsupported, OutcomeUnavailable, OutcomeError,
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
}

// UseCase processes operations.
type UseCase struct {
	deps Deps
}

// New builds the use case.
func New(d Deps) *UseCase {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &UseCase{deps: d}
}

// Execute processes the request. A Transient error (errors.Is
// application.ErrTemporarilyUnavailable) means nothing was stored: the caller may try
// again with the same key.
func (u *UseCase) Execute(ctx context.Context, in Input) (Output, error) {
	started := time.Now()
	out, outcome, err := u.execute(ctx, in)
	u.deps.Recorder.Handled(in.Request.Kind, outcome, time.Since(started))
	u.log(ctx, in, out, outcome, err)
	return out, err
}

func (u *UseCase) execute(ctx context.Context, in Input) (Output, Outcome, error) {
	request, err := wager.NewRequest(in.Request)
	if err != nil {
		return Output{}, OutcomeInvalid, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if request.Kind() != wager.KindBet {
		return Output{}, OutcomeUnsupported, fmt.Errorf("%w: %s", ErrUnsupportedKind, request.Kind())
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

	// 4. Decide and write.
	settlement, err := pending.Settle(locked, u.deps.NewID(), u.deps.Now())
	if err != nil {
		return Output{}, "", err
	}
	if settlement.Entry.IsValid() {
		if err := repos.Wallets().Update(ctx, locked, settlement.Wallet); err != nil {
			return Output{}, "", err
		}
		if err := repos.Ledger().Append(ctx, settlement.Entry); err != nil {
			return Output{}, "", err
		}
	}
	if err := transactions.Finish(ctx, settlement.Transaction); err != nil {
		return Output{}, "", err
	}
	if err := u.publish(ctx, repos, in, settlement); err != nil {
		return Output{}, "", err
	}

	outcome := OutcomeProcessed
	if settlement.Transaction.Status() == wager.StatusRejected {
		outcome = OutcomeRejected
	}
	return Output{Transaction: settlement.Transaction}, outcome, nil
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
	if err := u.publish(ctx, repos, in, wager.Settlement{Transaction: rejected}); err != nil {
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

// publish writes the events of a settlement to the outbox, in the same unit of work.
func (u *UseCase) publish(ctx context.Context, repos application.Repositories, in Input, s wager.Settlement) error {
	tx := s.Transaction
	external := tx.External()
	meta := func() event.Meta {
		return event.Meta{EventID: u.deps.NewID(), CorrelationID: tx.CorrelationID(), CausationID: in.CausationID, OccurredAt: tx.UpdatedAt()}
	}

	var events []event.Event
	if tx.Status() == wager.StatusRejected {
		rejected, err := event.NewWagerTransactionRejected(meta(), event.WagerTransactionRejectedData{
			TransactionID: tx.ID(), WalletID: tx.RequestedWalletID(), PlayerID: tx.PlayerID(), Kind: tx.Kind(),
			ProviderID: external.ProviderID, ExternalTransactionID: external.ExternalTransactionID,
			RoundID: external.RoundID, GameID: external.GameID, Money: tx.Amount(), FailureCode: tx.FailureCode(),
		})
		if err != nil {
			return err
		}
		events = append(events, rejected)
	} else {
		result, _ := tx.Result()
		processed, err := event.NewWagerTransactionProcessed(meta(), event.WagerTransactionProcessedData{
			TransactionID: tx.ID(), WalletID: tx.WalletID(), PlayerID: tx.PlayerID(), Kind: tx.Kind(), Origin: tx.Origin(),
			ProviderID: external.ProviderID, ExternalTransactionID: external.ExternalTransactionID,
			RoundID: external.RoundID, GameID: external.GameID,
			Money: tx.Amount(), Balance: result.Balance, WalletVersion: result.WalletVersion,
		})
		if err != nil {
			return err
		}
		events = append(events, processed)
	}
	if s.Entry.IsValid() {
		changed, err := event.NewWalletBalanceChanged(meta(), event.WalletBalanceChangedData{
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
	case OutcomeProcessed, OutcomeRejected, OutcomeReplayed, OutcomeDuplicateMessage:
		u.deps.Logger.InfoContext(ctx, "wager request handled", attrs...)
	case OutcomeKeyConflict, OutcomeDuplicateExternal, OutcomeMessageConflict, OutcomeInvalid, OutcomeUnsupported:
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
	case errors.Is(err, ErrUnsupportedKind):
		return "unsupported kind"
	case errors.Is(err, ErrInvalidInput):
		return "invalid input"
	}
	return "refused"
}
