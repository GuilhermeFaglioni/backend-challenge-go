package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

// TransactionReader reads Wager Transactions straight from the pool. It implements
// application.TransactionReader. The scope is part of the WHERE clause: what it does not
// reach is never read.
type TransactionReader struct {
	pool *pgxpool.Pool
}

func NewTransactionReader(pool *pgxpool.Pool) *TransactionReader {
	return &TransactionReader{pool: pool}
}

var _ application.TransactionReader = (*TransactionReader)(nil)

func (r *TransactionReader) ByID(ctx context.Context, id uuid.UUID, scope application.Scope) (wager.Transaction, error) {
	switch {
	case scope.All():
		return r.one(ctx, `WHERE id = $1`, id)
	case scope.ProviderID() != "":
		// origin = 'EXTERNAL' is implied by the provider id, which internal rows do not have.
		return r.one(ctx, `WHERE id = $1 AND origin = 'EXTERNAL' AND provider_id = $2`, id, scope.ProviderID())
	}
	return wager.Transaction{}, application.ErrTransactionNotFound
}

func (r *TransactionReader) ByExternalID(ctx context.Context, providerID, externalTransactionID string) (wager.Transaction, error) {
	return r.one(ctx, `WHERE origin = 'EXTERNAL' AND provider_id = $1 AND external_transaction_id = $2`, providerID, externalTransactionID)
}

func (r *TransactionReader) one(ctx context.Context, where string, args ...any) (wager.Transaction, error) {
	t, err := scanTransaction(r.pool.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions `+where, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return wager.Transaction{}, application.ErrTransactionNotFound
	}
	if err != nil {
		return wager.Transaction{}, markTransient(fmt.Errorf("read wager transaction: %w", err))
	}
	return t, nil
}
