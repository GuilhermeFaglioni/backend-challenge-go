package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

// LedgerAuditor reads a wallet's books for reconciliation. It implements
// application.LedgerAuditor.
type LedgerAuditor struct {
	pool *pgxpool.Pool
}

func NewLedgerAuditor(pool *pgxpool.Pool) *LedgerAuditor { return &LedgerAuditor{pool: pool} }

var _ application.LedgerAuditor = (*LedgerAuditor)(nil)

// Audit reads the stored balance and the ledger sums in one REPEATABLE READ, READ ONLY
// transaction: one snapshot, so a bet committed between the two reads cannot make the
// balance and the ledger disagree, and the transaction cannot write even by mistake.
func (a *LedgerAuditor) Audit(ctx context.Context, walletID uuid.UUID) (application.LedgerAudit, error) {
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.LedgerAudit{}, markTransient(fmt.Errorf("begin the audit: %w", err))
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The snapshot is taken by the first statement. Check it is the transaction we asked
	// for before trusting what it reads: a connection configured otherwise (a pooler, a
	// session default) would quietly turn the audit into something weaker.
	var isolation, readOnly string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Scan(&isolation, &readOnly); err != nil {
		return application.LedgerAudit{}, markTransient(fmt.Errorf("read the audit settings: %w", err))
	}
	if isolation != "repeatable read" || readOnly != "on" {
		return application.LedgerAudit{}, fmt.Errorf("the audit runs as %q, read only %s; it must be a repeatable read, read only transaction", isolation, readOnly)
	}

	var currency string
	var stored int64
	err = tx.QueryRow(ctx, `SELECT currency, balance_minor FROM wallets WHERE id = $1`, walletID).Scan(&currency, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.LedgerAudit{}, application.ErrWalletNotFound
	}
	if err != nil {
		return application.LedgerAudit{}, markTransient(fmt.Errorf("read the wallet: %w", err))
	}
	cur, err := money.ParseCurrency(currency)
	if err != nil {
		return application.LedgerAudit{}, fmt.Errorf("wallet %s: %w", walletID, err)
	}

	// sum(bigint) is numeric in PostgreSQL, so it cannot overflow; it is read as text and
	// kept as a big integer until it has been checked against the range of a balance.
	var credits, debits string
	var entries int64
	err = tx.QueryRow(ctx, `
		SELECT coalesce(sum(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0)::text,
		       coalesce(sum(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0)::text,
		       count(*)
		FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&credits, &debits, &entries)
	if err != nil {
		return application.LedgerAudit{}, markTransient(fmt.Errorf("sum the ledger: %w", err))
	}
	audit := application.LedgerAudit{Currency: cur, Stored: stored, Entries: entries}
	var ok1, ok2 bool
	audit.Credits, ok1 = new(big.Int).SetString(credits, 10)
	audit.Debits, ok2 = new(big.Int).SetString(debits, 10)
	if !ok1 || !ok2 {
		return application.LedgerAudit{}, fmt.Errorf("the ledger sums %q and %q are not numbers", credits, debits)
	}
	return audit, nil
}
