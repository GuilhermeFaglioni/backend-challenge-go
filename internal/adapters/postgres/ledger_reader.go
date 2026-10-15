package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// LedgerReader reads a wallet's ledger straight from the pool. It implements
// application.LedgerReader.
type LedgerReader struct {
	pool *pgxpool.Pool
}

func NewLedgerReader(pool *pgxpool.Pool) *LedgerReader { return &LedgerReader{pool: pool} }

var _ application.LedgerReader = (*LedgerReader)(nil)

func (r *LedgerReader) List(ctx context.Context, walletID uuid.UUID, after int64, limit int) ([]application.LedgerRecord, error) {
	// The ledger stores amounts only; the currency is the wallet's.
	rows, err := r.pool.Query(ctx, `
		SELECT e.seq, e.id, e.transaction_id, e.direction, e.amount_minor, e.balance_before_minor, e.balance_after_minor, e.created_at, w.currency
		FROM wallet_ledger_entries e
		JOIN wallets w ON w.id = e.wallet_id
		WHERE e.wallet_id = $1 AND e.seq > $2
		ORDER BY e.seq
		LIMIT $3`, walletID, after, limit)
	if err != nil {
		return nil, markTransient(fmt.Errorf("list the ledger: %w", err))
	}
	defer rows.Close()

	var records []application.LedgerRecord
	for rows.Next() {
		var (
			seq                    int64
			id, transactionID      uuid.UUID
			direction, currency    string
			amount, before, after_ int64
			createdAt              time.Time
		)
		if err := rows.Scan(&seq, &id, &transactionID, &direction, &amount, &before, &after_, &createdAt, &currency); err != nil {
			return nil, fmt.Errorf("read a ledger entry: %w", err)
		}
		cur, err := money.ParseCurrency(currency)
		if err != nil {
			return nil, fmt.Errorf("ledger of wallet %s: %w", walletID, err)
		}
		entry, err := newEntry(walletID, id, transactionID, direction, amount, before, after_, createdAt, cur)
		if err != nil {
			return nil, fmt.Errorf("ledger entry %s is not a valid entry: %w", id, err)
		}
		records = append(records, application.LedgerRecord{Seq: seq, Entry: entry})
	}
	if err := rows.Err(); err != nil {
		return nil, markTransient(fmt.Errorf("list the ledger: %w", err))
	}
	return records, nil
}

func newEntry(walletID, id, transactionID uuid.UUID, direction string, amount, before, after int64, createdAt time.Time, cur money.Currency) (wallet.LedgerEntry, error) {
	amountMoney, err := money.FromMinor(amount, cur)
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	beforeMoney, err := money.FromMinor(before, cur)
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	afterMoney, err := money.FromMinor(after, cur)
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	return wallet.NewLedgerEntry(wallet.LedgerEntryParams{
		ID: id, WalletID: walletID, TransactionID: transactionID, Direction: wallet.Direction(direction),
		Amount: amountMoney, BalanceBefore: beforeMoney, BalanceAfter: afterMoney, CreatedAt: createdAt,
	})
}
