package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
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

// --- wallets: lock and update -------------------------------------------------------

// Lock reads the wallet FOR NO KEY UPDATE. That lock serializes the writers of one
// wallet, but is compatible with the FOR KEY SHARE that inserts into wager_transactions
// and the ledger take on the wallet to check their foreign key; FOR UPDATE would
// conflict with those and could deadlock two operations on the same wallet
// (ARCHITECTURE.md, section 7).
func (r walletRepository) Lock(ctx context.Context, id uuid.UUID) (wallet.Wallet, error) {
	var (
		playerID  uuid.UUID
		currency  string
		balance   int64
		version   int64
		lastTx    *uuid.UUID
		createdAt time.Time
		updatedAt time.Time
	)
	err := r.tx.QueryRow(ctx, `
		SELECT player_id, currency, balance_minor, version, last_transaction_id, created_at, updated_at
		FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&playerID, &currency, &balance, &version, &lastTx, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return wallet.Wallet{}, application.ErrWalletNotFound
	}
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("lock wallet: %w", err)
	}
	return rehydrateWallet(id, playerID, currency, balance, version, lastTx, createdAt, updatedAt)
}

func (r walletRepository) Update(ctx context.Context, before, after wallet.Wallet) error {
	var lastTransaction any
	if id, has := after.LastTransactionID(); has {
		lastTransaction = id
	}
	tag, err := r.tx.Exec(ctx, `
		UPDATE wallets
		SET balance_minor = $3, version = $4, last_transaction_id = $5, updated_at = $6
		WHERE id = $1 AND version = $2`,
		after.ID(), before.Version(), after.Balance().Minor(), after.Version(), lastTransaction, after.UpdatedAt())
	if err != nil {
		return fmt.Errorf("update wallet: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return application.ErrConcurrentUpdate
	}
	return nil
}

// rehydrateWallet turns the columns of a wallet into the aggregate.
func rehydrateWallet(id, playerID uuid.UUID, currency string, balance, version int64, lastTx *uuid.UUID, createdAt, updatedAt time.Time) (wallet.Wallet, error) {
	cur, err := money.ParseCurrency(currency)
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("wallet %s has a currency the domain does not know: %w", id, err)
	}
	stored, err := money.FromMinor(balance, cur)
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("wallet %s balance: %w", id, err)
	}
	state := wallet.State{ID: id, PlayerID: playerID, Balance: stored, Version: version, CreatedAt: createdAt, UpdatedAt: updatedAt}
	if lastTx != nil {
		state.LastTransactionID = *lastTx
	}
	w, err := wallet.Rehydrate(state)
	if err != nil {
		return wallet.Wallet{}, fmt.Errorf("wallet %s is not a valid wallet: %w", id, err)
	}
	return w, nil
}

// --- wager transactions: lookups, claim and outcome --------------------------------

// transactionColumns is the column list every lookup reads, in the order scanTransaction expects.
const transactionColumns = `id, kind, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, failure_code,
	result_balance_minor, result_wallet_version, provider_id, external_transaction_id, idempotency_key, payload_hash,
	round_id, game_id, reference_external_transaction_id, reference_transaction_id, correlation_id,
	created_at, updated_at, processed_at`

func (r transactionRepository) FindByKey(ctx context.Context, providerID, idempotencyKey string) (wager.Transaction, bool, error) {
	return r.find(ctx, `WHERE origin = 'EXTERNAL' AND provider_id = $1 AND idempotency_key = $2`, providerID, idempotencyKey)
}

func (r transactionRepository) FindByExternalID(ctx context.Context, providerID, externalTransactionID string) (wager.Transaction, bool, error) {
	return r.find(ctx, `WHERE origin = 'EXTERNAL' AND provider_id = $1 AND external_transaction_id = $2`, providerID, externalTransactionID)
}

func (r transactionRepository) find(ctx context.Context, where string, args ...any) (wager.Transaction, bool, error) {
	row := r.tx.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions `+where, args...)
	t, err := scanTransaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return wager.Transaction{}, false, nil
	}
	if err != nil {
		return wager.Transaction{}, false, err
	}
	return t, true, nil
}

func scanTransaction(row pgx.Row) (wager.Transaction, error) {
	var (
		s                        wager.State
		kind, currency, status   string
		failure                  *string
		walletID                 *uuid.UUID
		resultBalance, resultVer *int64
		amount                   int64
		providerID, externalID   *string
		key, hash, round, game   *string
		referenceExternal        *string
		referenceID              *uuid.UUID
		correlation              *string
		processedAt              *time.Time
	)
	if err := row.Scan(&s.ID, &kind, &s.RequestedWalletID, &walletID, &s.PlayerID, &amount, &currency, &status, &failure,
		&resultBalance, &resultVer, &providerID, &externalID, &key, &hash, &round, &game, &referenceExternal, &referenceID,
		&correlation, &s.CreatedAt, &s.UpdatedAt, &processedAt); err != nil {
		return wager.Transaction{}, err
	}

	cur, err := money.ParseCurrency(currency)
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("transaction %s: %w", s.ID, err)
	}
	if s.Amount, err = money.FromMinor(amount, cur); err != nil {
		return wager.Transaction{}, fmt.Errorf("transaction %s: %w", s.ID, err)
	}
	s.Kind, s.Status = wager.Kind(kind), wager.Status(status)
	s.FailureCode = wager.FailureCode(deref(failure))
	if walletID != nil {
		s.WalletID = *walletID
	}
	if resultBalance != nil && resultVer != nil {
		balance, err := money.FromMinor(*resultBalance, cur)
		if err != nil {
			return wager.Transaction{}, fmt.Errorf("transaction %s result: %w", s.ID, err)
		}
		s.Result = &wager.Result{Balance: balance, WalletVersion: *resultVer}
	}
	s.External = wager.External{
		ProviderID: deref(providerID), ExternalTransactionID: deref(externalID), IdempotencyKey: deref(key),
		PayloadHash: wager.PayloadHash(deref(hash)), RoundID: deref(round), GameID: deref(game),
		ReferenceExternalTransactionID: deref(referenceExternal),
	}
	if referenceID != nil {
		s.External.ReferenceTransactionID = *referenceID
	}
	s.CorrelationID = deref(correlation)
	s.ProcessedAt = processedAt

	t, err := wager.Rehydrate(s)
	if err != nil {
		return wager.Transaction{}, fmt.Errorf("transaction %s is not a valid transaction: %w", s.ID, err)
	}
	return t, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Claim inserts the transaction unless its key or its external id is taken.
// ON CONFLICT DO NOTHING (with no target, so either unique index counts) waits for a
// concurrent insert of the same key to commit or roll back, which makes the answer
// final; and it does not abort the database transaction the way a violation would,
// so the existing row can still be read afterwards.
func (r transactionRepository) Claim(ctx context.Context, t wager.Transaction) (bool, error) {
	external := t.External()
	result, hasResult := t.Result()
	processedAt, hasProcessedAt := t.ProcessedAt()

	var walletID, resultBalance, resultVersion, referenceExternal, referenceID, failure, processed any
	if id := t.WalletID(); id != uuid.Nil {
		walletID = id
	}
	if hasResult {
		resultBalance, resultVersion = result.Balance.Minor(), result.WalletVersion
	}
	if external.ReferenceExternalTransactionID != "" {
		referenceExternal = external.ReferenceExternalTransactionID
	}
	if external.ReferenceTransactionID != uuid.Nil {
		referenceID = external.ReferenceTransactionID
	}
	if code := t.FailureCode(); code != "" {
		failure = string(code)
	}
	if hasProcessedAt {
		processed = processedAt
	}

	tag, err := r.tx.Exec(ctx, `
		INSERT INTO wager_transactions
			(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash,
			 requested_wallet_id, wallet_id, player_id, round_id, game_id, amount_minor, currency,
			 reference_external_transaction_id, reference_transaction_id, status, failure_code,
			 result_balance_minor, result_wallet_version, correlation_id, created_at, updated_at, processed_at)
		VALUES ($1, 'EXTERNAL', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		ON CONFLICT DO NOTHING`,
		t.ID(), string(t.Kind()), external.ProviderID, external.ExternalTransactionID, external.IdempotencyKey, string(external.PayloadHash),
		t.RequestedWalletID(), walletID, t.PlayerID(), external.RoundID, external.GameID, t.Amount().Minor(), t.Amount().Currency().String(),
		referenceExternal, referenceID, string(t.Status()), failure, resultBalance, resultVersion, t.CorrelationID(),
		t.CreatedAt(), t.UpdatedAt(), processed)
	if err != nil {
		return false, fmt.Errorf("claim wager transaction: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r transactionRepository) Finish(ctx context.Context, t wager.Transaction) error {
	result, hasResult := t.Result()
	processedAt, hasProcessedAt := t.ProcessedAt()
	var resultBalance, resultVersion, failure, processed any
	if hasResult {
		resultBalance, resultVersion = result.Balance.Minor(), result.WalletVersion
	}
	if code := t.FailureCode(); code != "" {
		failure = string(code)
	}
	if hasProcessedAt {
		processed = processedAt
	}
	tag, err := r.tx.Exec(ctx, `
		UPDATE wager_transactions
		SET status = $2, failure_code = $3, result_balance_minor = $4, result_wallet_version = $5, updated_at = $6, processed_at = $7
		WHERE id = $1`,
		t.ID(), string(t.Status()), failure, resultBalance, resultVersion, t.UpdatedAt(), processed)
	if err != nil {
		return fmt.Errorf("finish wager transaction: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("finish wager transaction %s: it was not claimed in this unit of work", t.ID())
	}
	return nil
}
