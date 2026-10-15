package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// WalletReader reads wallets straight from the pool. It implements
// application.WalletReader.
type WalletReader struct {
	pool *pgxpool.Pool
}

func NewWalletReader(pool *pgxpool.Pool) *WalletReader { return &WalletReader{pool: pool} }

var _ application.WalletReader = (*WalletReader)(nil)

func (r *WalletReader) ByID(ctx context.Context, id uuid.UUID) (wallet.Wallet, error) {
	var (
		playerID  uuid.UUID
		currency  string
		balance   int64
		version   int64
		lastTx    *uuid.UUID
		createdAt time.Time
		updatedAt time.Time
	)
	err := r.pool.QueryRow(ctx, `
		SELECT player_id, currency, balance_minor, version, last_transaction_id, created_at, updated_at
		FROM wallets WHERE id = $1`, id).Scan(&playerID, &currency, &balance, &version, &lastTx, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return wallet.Wallet{}, application.ErrWalletNotFound
	}
	if err != nil {
		return wallet.Wallet{}, markTransient(fmt.Errorf("read wallet: %w", err))
	}

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
