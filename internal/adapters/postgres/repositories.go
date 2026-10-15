package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// repositories are bound to one database transaction; they never begin or end one.
type repositories struct {
	tx pgx.Tx
}

func newRepositories(tx pgx.Tx) *repositories { return &repositories{tx: tx} }

func (r *repositories) Wallets() application.WalletRepository { return walletRepository{r.tx} }
func (r *repositories) Transactions() application.TransactionRepository {
	return transactionRepository{r.tx}
}
func (r *repositories) Ledger() application.LedgerRepository { return ledgerRepository{r.tx} }
func (r *repositories) Outbox() application.OutboxRepository { return outboxRepository{r.tx} }

// --- wallets -----------------------------------------------------------------------

type walletRepository struct{ tx pgx.Tx }

// Insert stores the wallet. ON CONFLICT DO NOTHING rather than catching the unique
// violation: a failed statement would abort the whole database transaction, and the
// existing wallet's id has to be read afterwards.
func (r walletRepository) Insert(ctx context.Context, w wallet.Wallet) error {
	var lastTransaction any
	if id, has := w.LastTransactionID(); has {
		lastTransaction = id
	}
	tag, err := r.tx.Exec(ctx, `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, last_transaction_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (player_id, currency) DO NOTHING`,
		w.ID(), w.PlayerID(), w.Currency().String(), w.Balance().Minor(), w.Version(), lastTransaction, w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		return fmt.Errorf("insert wallet: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	var existing uuid.UUID
	err = r.tx.QueryRow(ctx, `SELECT id FROM wallets WHERE player_id = $1 AND currency = $2`,
		w.PlayerID(), w.Currency().String()).Scan(&existing)
	if err != nil {
		return fmt.Errorf("read the existing wallet: %w", err)
	}
	return &application.WalletAlreadyExistsError{WalletID: existing}
}

// --- wager transactions ------------------------------------------------------------

type transactionRepository struct{ tx pgx.Tx }

func (r transactionRepository) InsertOpening(ctx context.Context, t wager.Transaction) error {
	result, processed := t.Result()
	processedAt, hasProcessedAt := t.ProcessedAt()
	var resultBalance, resultVersion, processedAtValue any
	if processed {
		resultBalance, resultVersion = result.Balance.Minor(), result.WalletVersion
	}
	if hasProcessedAt {
		processedAtValue = processedAt
	}
	var failure any
	if code := t.FailureCode(); code != "" {
		failure = string(code)
	}

	_, err := r.tx.Exec(ctx, `
		INSERT INTO wager_transactions
			(id, origin, kind, requested_wallet_id, wallet_id, player_id, amount_minor, currency,
			 status, failure_code, result_balance_minor, result_wallet_version, correlation_id,
			 created_at, updated_at, processed_at)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		t.ID(), string(t.Origin()), string(t.Kind()), t.WalletID(), t.PlayerID(),
		t.Amount().Minor(), t.Amount().Currency().String(),
		string(t.Status()), failure, resultBalance, resultVersion, t.CorrelationID(),
		t.CreatedAt(), t.UpdatedAt(), processedAtValue)
	if err != nil {
		return fmt.Errorf("insert wager transaction: %w", err)
	}
	return nil
}

// --- ledger ------------------------------------------------------------------------

type ledgerRepository struct{ tx pgx.Tx }

func (r ledgerRepository) Append(ctx context.Context, e wallet.LedgerEntry) error {
	_, err := r.tx.Exec(ctx, `
		INSERT INTO wallet_ledger_entries
			(id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.ID(), e.WalletID(), e.TransactionID(), string(e.Direction()),
		e.Amount().Minor(), e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.CreatedAt())
	if err != nil {
		return fmt.Errorf("append ledger entry: %w", err)
	}
	return nil
}

// --- outbox ------------------------------------------------------------------------

type outboxRepository struct{ tx pgx.Tx }

func (r outboxRepository) Add(ctx context.Context, e event.Event) error {
	payload, err := e.DataJSON()
	if err != nil {
		return fmt.Errorf("serialize event %s: %w", e.EventID(), err)
	}
	var causation any
	if c := e.CausationID(); c != "" {
		causation = c
	}
	_, err = r.tx.Exec(ctx, `
		INSERT INTO outbox (event_id, aggregate_id, event_type, version, payload, correlation_id, causation_id, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.EventID(), e.AggregateID(), e.Type(), e.Version(), payload, e.CorrelationID(), causation, e.OccurredAt())
	if err != nil {
		return fmt.Errorf("add event %s to the outbox: %w", e.EventID(), err)
	}
	return nil
}
