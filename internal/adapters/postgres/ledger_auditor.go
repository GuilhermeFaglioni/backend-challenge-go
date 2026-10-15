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

// Audit reads the stored balance, the last checkpoint and the ledger sums in one
// REPEATABLE READ, READ ONLY transaction: one snapshot, so a bet committed between the
// reads cannot make the balance and the ledger disagree, and the transaction cannot write
// even by mistake.
func (a *LedgerAuditor) Audit(ctx context.Context, walletID uuid.UUID, full bool) (application.LedgerAudit, error) {
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

	audit := application.LedgerAudit{Currency: cur, Stored: stored}
	checkpoint, err := latestCheckpoint(ctx, tx, walletID)
	if err != nil {
		return application.LedgerAudit{}, err
	}
	after := int64(0)
	if full {
		audit.Latest = checkpoint
	} else if checkpoint != nil {
		audit.Base = checkpoint
		after = checkpoint.ThroughSeq
	}
	// The checkpoint's seq bounds the prefix a full audit checks it against; with none,
	// the prefix is empty and not reported.
	prefixThrough := int64(0)
	if audit.Latest != nil {
		prefixThrough = audit.Latest.ThroughSeq
	}

	// sum(bigint) is numeric in PostgreSQL, so it cannot overflow; it is read as text and
	// kept as a big integer until it has been checked against the range of a balance.
	var credits, debits, prefixCredits, prefixDebits string
	var prefixEntries int64
	err = tx.QueryRow(ctx, `
		SELECT coalesce(sum(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0)::text,
		       coalesce(sum(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0)::text,
		       count(*),
		       coalesce(max(seq), $2),
		       coalesce(sum(amount_minor) FILTER (WHERE direction = 'CREDIT' AND seq <= $3), 0)::text,
		       coalesce(sum(amount_minor) FILTER (WHERE direction = 'DEBIT' AND seq <= $3), 0)::text,
		       count(*) FILTER (WHERE seq <= $3)
		FROM wallet_ledger_entries WHERE wallet_id = $1 AND seq > $2`, walletID, after, prefixThrough).
		Scan(&credits, &debits, &audit.Entries, &audit.LastSeq, &prefixCredits, &prefixDebits, &prefixEntries)
	if err != nil {
		return application.LedgerAudit{}, markTransient(fmt.Errorf("sum the ledger: %w", err))
	}
	if audit.Credits, err = bigSum(credits); err != nil {
		return application.LedgerAudit{}, err
	}
	if audit.Debits, err = bigSum(debits); err != nil {
		return application.LedgerAudit{}, err
	}
	if audit.Latest != nil {
		prefix := application.LedgerSums{Entries: prefixEntries}
		if prefix.Credits, err = bigSum(prefixCredits); err != nil {
			return application.LedgerAudit{}, err
		}
		if prefix.Debits, err = bigSum(prefixDebits); err != nil {
			return application.LedgerAudit{}, err
		}
		audit.Prefix = &prefix
	}
	return audit, nil
}

// latestCheckpoint is the wallet's checkpoint with the highest seq, or nil.
func latestCheckpoint(ctx context.Context, tx pgx.Tx, walletID uuid.UUID) (*application.LedgerCheckpoint, error) {
	var c application.LedgerCheckpoint
	var credits, debits string
	err := tx.QueryRow(ctx, `
		SELECT through_seq, credits_minor::text, debits_minor::text, entries
		FROM reconciliation_checkpoints WHERE wallet_id = $1
		ORDER BY through_seq DESC LIMIT 1`, walletID).Scan(&c.ThroughSeq, &credits, &debits, &c.Entries)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, markTransient(fmt.Errorf("read the last checkpoint: %w", err))
	}
	if c.Credits, err = bigSum(credits); err != nil {
		return nil, err
	}
	if c.Debits, err = bigSum(debits); err != nil {
		return nil, err
	}
	return &c, nil
}

func bigSum(text string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, fmt.Errorf("the ledger sum %q is not a number", text)
	}
	return n, nil
}

// RecordCheckpoint calls record_reconciliation_checkpoint (migration 000014), the only way
// the application role can write a checkpoint: the function computes the sums from the
// ledger itself.
func (a *LedgerAuditor) RecordCheckpoint(ctx context.Context, walletID uuid.UUID, throughSeq int64) (bool, error) {
	var recorded bool
	if err := a.pool.QueryRow(ctx, `SELECT record_reconciliation_checkpoint($1, $2)`, walletID, throughSeq).Scan(&recorded); err != nil {
		return false, markTransient(fmt.Errorf("record the checkpoint: %w", err))
	}
	return recorded, nil
}
