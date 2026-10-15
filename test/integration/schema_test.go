//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// Every constraint and trigger of the schema is tried here by direct SQL, as the
// table owner (the strictest case: the application role holds fewer privileges),
// and must refuse. The point is that the database holds the financial invariants on
// its own, whatever the application does or fails to do (ADR 0002).

const (
	uniqueViolation = "23505"
	checkViolation  = "23514"
	foreignKeyError = "23503"
	restrictError   = "23001"
)

type schema struct {
	ctx    context.Context
	owner  *pgx.Conn
	app    *pgx.Conn
	appDSN string
}

func newSchema(t *testing.T) *schema {
	t.Helper()
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))
	return &schema{ctx: ctx, owner: connect(ctx, t, db.OwnerDSN), app: connect(ctx, t, db.AppDSN), appDSN: db.AppDSN}
}

// exec runs the statement on the connection and returns the SQLSTATE of its failure
// ("" on success).
func (s *schema) exec(c *pgx.Conn, sql string, args ...any) string {
	_, err := c.Exec(s.ctx, sql, args...)
	return sqlState(err)
}

// inTx runs the statements in one database transaction and returns the SQLSTATE of
// the first failure, the commit included (the coherence triggers fire there).
func (s *schema) inTx(t *testing.T, c *pgx.Conn, statements ...func(pgx.Tx) error) string {
	t.Helper()
	tx, err := c.Begin(s.ctx)
	require.NoError(t, err)
	for _, statement := range statements {
		if err := statement(tx); err != nil {
			_ = tx.Rollback(s.ctx)
			return sqlState(err)
		}
	}
	err = tx.Commit(s.ctx)
	if err != nil {
		return sqlState(err)
	}
	return ""
}

func run(sql string, args ...any) func(pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	}
}

var now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// wallet inserts an empty wallet.
func (s *schema) wallet(t *testing.T) (walletID, playerID uuid.UUID) {
	t.Helper()
	walletID, playerID = uuid.New(), uuid.New()
	require.Empty(t, s.exec(s.owner, `INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, 'BRL', 0, 1, $3, $3)`, walletID, playerID, now))
	return walletID, playerID
}

// external inserts an external Wager Transaction and returns its id.
func (s *schema) external(t *testing.T, walletID, playerID uuid.UUID, provider, key, externalID, kind string, amount int64, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	failure := any(nil)
	if status == "REJECTED" || status == "FAILED" {
		failure = "SOME_FAILURE"
	}
	require.Empty(t, s.exec(s.owner, `INSERT INTO wager_transactions
		(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash,
		 requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, failure_code, created_at, updated_at)
		VALUES ($1, 'EXTERNAL', $2, $3, $4, $5, 'hash', $6, $6, $7, $8, 'BRL', $9, $10, $11, $11)`,
		id, kind, provider, externalID, key, walletID, playerID, amount, status, failure, now))
	return id
}

// credit moves a wallet from its current balance by a credit, as a correct
// application would: transaction, entry and wallet in one database transaction.
func (s *schema) credit(t *testing.T, walletID, playerID uuid.UUID, before, amount, version int64) uuid.UUID {
	t.Helper()
	txID := uuid.New()
	state := s.inTx(t, s.owner,
		run(`INSERT INTO wager_transactions
			(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash,
			 requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', 'WIN', 'p', $1::uuid::text, $1::uuid::text, 'h', $2, $2, $3, $4, 'BRL', 'PROCESSED', $5, $5)`,
			txID, walletID, playerID, amount, now),
		run(`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
			VALUES ($1, $2, $3, 'CREDIT', $4, $5, $6, $7)`, uuid.New(), walletID, txID, amount, before, before+amount, now),
		run(`UPDATE wallets SET balance_minor = $2, version = $3, last_transaction_id = $4, updated_at = $5 WHERE id = $1`,
			walletID, before+amount, version, txID, now),
	)
	require.Empty(t, state, "a correct movement must commit")
	return txID
}

// disable turns a trigger off for the rest of the test, so that the barrier under
// test is the only one standing. The barriers overlap by design (defence in depth),
// and without this a test passes on whichever one fires first, hiding a missing one.
func (s *schema) disable(t *testing.T, table, trigger string) {
	t.Helper()
	require.Empty(t, s.exec(s.owner, `ALTER TABLE `+table+` DISABLE TRIGGER `+trigger))
}

// --- wallets -----------------------------------------------------------------------

func TestWalletsConstraints(t *testing.T) {
	s := newSchema(t)
	insert := `INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`

	t.Run("one wallet per player and currency", func(t *testing.T) {
		_, player := s.wallet(t)
		require.Equal(t, uniqueViolation, s.exec(s.owner, insert, uuid.New(), player, "BRL", 0, 1, now))
		require.Empty(t, s.exec(s.owner, insert, uuid.New(), player, "USD", 0, 1, now), "another currency is another wallet")
	})
	t.Run("the balance is never negative", func(t *testing.T) {
		// The CHECK is the last barrier, behind the ledger coherence trigger: alone.
		s.disable(t, "wallets", "wallets_ledger_guard")
		require.Equal(t, checkViolation, s.exec(s.owner, insert, uuid.New(), uuid.New(), "BRL", -1, 1, now))
	})
	t.Run("only supported currencies", func(t *testing.T) {
		require.Equal(t, checkViolation, s.exec(s.owner, insert, uuid.New(), uuid.New(), "JPY", 0, 1, now))
	})
	t.Run("the version is at least 1", func(t *testing.T) {
		require.Equal(t, checkViolation, s.exec(s.owner, insert, uuid.New(), uuid.New(), "BRL", 0, 0, now))
	})
	t.Run("a wallet is inserted at version exactly 1", func(t *testing.T) {
		require.Equal(t, checkViolation, s.exec(s.owner, insert, uuid.New(), uuid.New(), "BRL", 0, 2, now))
	})
}

func TestTheVersionGuard(t *testing.T) {
	s := newSchema(t)
	id, player := s.wallet(t)
	s.credit(t, id, player, 0, 1000, 2) // balance 1000, version 2
	// The guard under test, alone: the ledger coherence trigger would also refuse
	// these updates, for lack of an entry.
	s.disable(t, "wallets", "wallets_ledger_guard")

	t.Run("a balance change must raise the version by exactly one", func(t *testing.T) {
		for _, version := range []int64{2, 4} {
			state := s.inTx(t, s.owner, run(`UPDATE wallets SET balance_minor = 900, version = $2, last_transaction_id = $3 WHERE id = $1`, id, version, uuid.New()))
			require.Equal(t, checkViolation, state, "version %d", version)
		}
	})
	t.Run("a balance change must name a new last transaction", func(t *testing.T) {
		state := s.inTx(t, s.owner, run(`UPDATE wallets SET balance_minor = 900, version = 3 WHERE id = $1`, id))
		require.Equal(t, checkViolation, state, "unchanged last transaction")
		state = s.inTx(t, s.owner, run(`UPDATE wallets SET balance_minor = 900, version = 3, last_transaction_id = NULL WHERE id = $1`, id))
		require.Equal(t, checkViolation, state, "no last transaction")
	})
	t.Run("the version moves only with the balance", func(t *testing.T) {
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wallets SET version = 3 WHERE id = $1`, id))
	})
	t.Run("a change without a balance change is allowed", func(t *testing.T) {
		require.Empty(t, s.exec(s.owner, `UPDATE wallets SET updated_at = $2 WHERE id = $1`, id, now.Add(time.Hour)))
	})
	t.Run("identity never changes", func(t *testing.T) {
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wallets SET player_id = $2 WHERE id = $1`, id, uuid.New()))
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wallets SET currency = 'USD' WHERE id = $1`, id))
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wallets SET id = $2 WHERE id = $1`, id, uuid.New()))
	})
}

// --- wager transactions ------------------------------------------------------------

func TestWagerTransactionsOriginCheck(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)

	t.Run("an opening cannot carry external fields", func(t *testing.T) {
		state := s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'INTERNAL', 'OPENING', 'p', $2, $2, $3, 100, 'BRL', 'PROCESSED', $4, $4)`, uuid.New(), wallet, player, now)
		require.Equal(t, checkViolation, state)
	})
	t.Run("an internal request cannot be anything but an opening", func(t *testing.T) {
		state := s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'INTERNAL', 'BET', $2, $2, $3, 100, 'BRL', 'PROCESSED', $4, $4)`, uuid.New(), wallet, player, now)
		require.Equal(t, checkViolation, state)
	})
	t.Run("an external request cannot be an opening", func(t *testing.T) {
		state := s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', 'OPENING', 'p', 'e', 'k', 'h', $2, $2, $3, 100, 'BRL', 'PROCESSED', $4, $4)`, uuid.New(), wallet, player, now)
		require.Equal(t, checkViolation, state)
	})
	t.Run("an external request needs provider, id, key and hash", func(t *testing.T) {
		for _, missing := range []string{"provider_id", "external_transaction_id", "idempotency_key", "payload_hash"} {
			fields := map[string]string{"provider_id": "'p'", "external_transaction_id": "'e'", "idempotency_key": "'k'", "payload_hash": "'h'"}
			fields[missing] = "NULL"
			state := s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
				VALUES ($1, 'EXTERNAL', 'BET', `+fields["provider_id"]+`, `+fields["external_transaction_id"]+`, `+fields["idempotency_key"]+`, `+fields["payload_hash"]+`, $2, $2, $3, 100, 'BRL', 'PROCESSED', $4, $4)`,
				uuid.New(), wallet, player, now)
			require.Equal(t, checkViolation, state, "missing %s", missing)
		}
	})
	t.Run("an unknown wallet id is a foreign key error", func(t *testing.T) {
		state := s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', 'BET', 'p', 'e2', 'k2', 'h', $2, $2, $3, 100, 'BRL', 'PROCESSED', $4, $4)`, uuid.New(), uuid.New(), player, now)
		require.Equal(t, foreignKeyError, state)
	})
}

func TestWagerTransactionsIdentityIsUniquePerProvider(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	s.external(t, wallet, player, "provider-a", "key-1", "ext-1", "BET", 100, "PROCESSED")

	insert := func(provider, key, externalID string) string {
		return s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', 'BET', $2, $4, $3, 'h', $5, $5, $6, 100, 'BRL', 'PROCESSED', $7, $7)`,
			uuid.New(), provider, key, externalID, wallet, player, now)
	}
	require.Equal(t, uniqueViolation, insert("provider-a", "key-1", "ext-2"), "same provider, same key")
	require.Equal(t, uniqueViolation, insert("provider-a", "key-2", "ext-1"), "same provider, same external id with another key")
	require.Empty(t, insert("provider-b", "key-1", "ext-1"), "another provider owns its own keys and ids")
}

func TestOnlyOneOpeningPerWallet(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	opening := func() string {
		return s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'INTERNAL', 'OPENING', $2, $2, $3, 100, 'BRL', 'PROCESSED', $4, $4)`, uuid.New(), wallet, player, now)
	}
	require.Empty(t, opening())
	require.Equal(t, uniqueViolation, opening(), "a second initial credit for the same wallet")
}

func TestWagerTransactionsAmountRulesPerKind(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	insert := func(kind string, amount int64) string {
		id := uuid.New()
		return s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', $2, 'p', $1::uuid::text, $1::uuid::text, 'h', $3, $3, $4, $5, 'BRL', 'PROCESSED', $6, $6)`, id, kind, wallet, player, amount, now)
	}
	for _, kind := range []string{"BET", "WIN", "REFUND", "ROLLBACK"} {
		require.Equal(t, checkViolation, insert(kind, 0), "%s needs a positive amount", kind)
		require.Equal(t, checkViolation, insert(kind, -5), "%s needs a positive amount", kind)
		require.Empty(t, insert(kind, 100), kind)
	}
	require.Equal(t, checkViolation, insert("LOSS", 100), "LOSS moves nothing")
	require.Empty(t, insert("LOSS", 0))
}

func TestOnlyAWalletNotFoundRejectionMayHaveNoWallet(t *testing.T) {
	s := newSchema(t)
	_, player := s.wallet(t)
	insert := func(status string, failure any) string {
		id := uuid.New()
		return s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, failure_code, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', 'BET', 'p', $1::uuid::text, $1::uuid::text, 'h', $2, NULL, $3, 100, 'BRL', $4, $5, $6, $6)`, id, uuid.New(), player, status, failure, now)
	}
	require.Equal(t, checkViolation, insert("PROCESSED", nil), "money cannot move without a wallet")
	require.Equal(t, checkViolation, insert("REJECTED", "INSUFFICIENT_FUNDS"), "other rejections have a wallet")
	require.Equal(t, checkViolation, insert("PENDING_REFERENCE", nil))
	require.Empty(t, insert("REJECTED", "WALLET_NOT_FOUND"))
}

func TestOnlyRejectionsAndFailuresCarryAFailureCode(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	insert := func(status string, failure any) string {
		id := uuid.New()
		return s.exec(s.owner, `INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, failure_code, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', 'BET', 'p', $1::uuid::text, $1::uuid::text, 'h', $2, $2, $3, 100, 'BRL', $4, $5, $6, $6)`, id, wallet, player, status, failure, now)
	}
	require.Equal(t, checkViolation, insert("PROCESSED", "X"))
	require.Equal(t, checkViolation, insert("REJECTED", nil))
	require.Equal(t, checkViolation, insert("FAILED", nil))
	require.Equal(t, checkViolation, insert("DONE", nil), "unknown status")
	require.Empty(t, insert("FAILED", "INTERNAL_PERMANENT_FAILURE"))
}

func TestWagerTransactionsStateAndIdentityAreProtected(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)

	t.Run("a terminal transaction never changes again", func(t *testing.T) {
		for _, status := range []string{"PROCESSED", "REJECTED", "FAILED"} {
			id := s.external(t, wallet, player, "p-"+status, "k", "e", "BET", 100, status)
			require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wager_transactions SET status = 'PENDING_REFERENCE' WHERE id = $1`, id), status)
			require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wager_transactions SET updated_at = $2 WHERE id = $1`, id, now.Add(time.Hour)), status)
			require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wager_transactions SET result_balance_minor = 5 WHERE id = $1`, id), status)
		}
	})
	t.Run("identity and money never change, even while pending", func(t *testing.T) {
		id := s.external(t, wallet, player, "p-pending", "k", "e", "BET", 100, "PENDING_REFERENCE")
		for _, change := range []string{
			`amount_minor = 999`, `currency = 'USD'`, `kind = 'WIN'`, `provider_id = 'other'`,
			`external_transaction_id = 'other'`, `idempotency_key = 'other'`, `payload_hash = 'other'`,
			`player_id = gen_random_uuid()`, `requested_wallet_id = gen_random_uuid()`, `wallet_id = NULL`, `origin = 'INTERNAL'`,
		} {
			state := s.exec(s.owner, `UPDATE wager_transactions SET `+change+` WHERE id = $1`, id)
			require.NotEmpty(t, state, "%s must be refused", change)
		}
	})
	t.Run("a pending reference cannot go back to pending", func(t *testing.T) {
		id := s.external(t, wallet, player, "p-back", "k", "e", "BET", 100, "PENDING_REFERENCE")
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wager_transactions SET status = 'PENDING' WHERE id = $1`, id))
	})
	t.Run("a pending reference can be finished once", func(t *testing.T) {
		id := s.external(t, wallet, player, "p-ok", "k", "e", "BET", 100, "PENDING_REFERENCE")
		require.Empty(t, s.exec(s.owner, `UPDATE wager_transactions SET updated_at = $2 WHERE id = $1`, id, now.Add(time.Minute)), "bookkeeping while waiting")
		require.Empty(t, s.exec(s.owner, `UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'REFERENCE_NOT_FOUND', updated_at = $2, processed_at = $2 WHERE id = $1`, id, now.Add(time.Hour)))
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE wager_transactions SET status = 'PROCESSED', failure_code = NULL WHERE id = $1`, id), "and no more")
	})
}

// --- the ledger --------------------------------------------------------------------

func TestLedgerRowConstraints(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	txID := s.external(t, wallet, player, "p", "k", "e", "WIN", 100, "PROCESSED")
	// The row constraints alone: the commit-time guard would refuse these rows too.
	s.disable(t, "wallet_ledger_entries", "wallet_ledger_entries_wallet_guard")
	insert := func(txID uuid.UUID, direction string, amount, before, after int64) string {
		return s.exec(s.owner, `INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, uuid.New(), wallet, txID, direction, amount, before, after, now)
	}

	require.Equal(t, checkViolation, insert(txID, "SIDEWAYS", 100, 0, 100), "direction")
	require.Equal(t, checkViolation, insert(txID, "CREDIT", 0, 100, 100), "amount must be positive")
	require.Equal(t, checkViolation, insert(txID, "CREDIT", -5, 100, 95), "amount must be positive")
	require.Equal(t, checkViolation, insert(txID, "CREDIT", 100, 50, 100), "credit adds")
	require.Equal(t, checkViolation, insert(txID, "DEBIT", 100, 200, 300), "debit subtracts")
	require.Equal(t, checkViolation, insert(txID, "DEBIT", 100, 50, -50), "no negative balance")
	require.Equal(t, checkViolation, insert(txID, "CREDIT", 100, -10, 90), "no negative balance")
	require.Equal(t, foreignKeyError, insert(uuid.New(), "CREDIT", 100, 0, 100), "the transaction must exist")
}

// A wallet and an entry that agree are committed together; one entry per wallet and
// transaction.
func TestLedgerUniquePerWalletAndTransaction(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	tx := s.credit(t, wallet, player, 0, 1000, 2)
	state := s.inTx(t, s.owner, run(`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
		VALUES ($1, $2, $3, 'CREDIT', 1000, 0, 1000, $4)`, uuid.New(), wallet, tx, now))
	require.Equal(t, uniqueViolation, state)
}

func TestLedgerIsAppendOnly(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	s.credit(t, wallet, player, 0, 1000, 2)

	t.Run("not even the owner can edit, delete or truncate", func(t *testing.T) {
		require.Equal(t, restrictError, s.exec(s.owner, `UPDATE wallet_ledger_entries SET created_at = created_at`))
		require.Equal(t, restrictError, s.exec(s.owner, `DELETE FROM wallet_ledger_entries`))
		require.Equal(t, restrictError, s.exec(s.owner, `TRUNCATE wallet_ledger_entries`))
	})
	t.Run("the application role is refused before it gets that far", func(t *testing.T) {
		require.Equal(t, insufficientPrivilege, s.exec(s.app, `UPDATE wallet_ledger_entries SET created_at = created_at`))
		require.Equal(t, insufficientPrivilege, s.exec(s.app, `DELETE FROM wallet_ledger_entries`))
		require.Equal(t, insufficientPrivilege, s.exec(s.app, `TRUNCATE wallet_ledger_entries`))
	})
	t.Run("the application role can read and append", func(t *testing.T) {
		var n int
		require.NoError(t, s.app.QueryRow(s.ctx, `SELECT count(*) FROM wallet_ledger_entries`).Scan(&n))
		require.Equal(t, 1, n)
		other, otherPlayer := s.wallet(t)
		tx := uuid.New()
		state := s.inTx(t, s.app,
			run(`INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
				VALUES ($1, 'EXTERNAL', 'WIN', 'p', $1::uuid::text, $1::uuid::text, 'h', $2, $2, $3, 50, 'BRL', 'PROCESSED', $4, $4)`, tx, other, otherPlayer, now),
			run(`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
				VALUES ($1, $2, $3, 'CREDIT', 50, 0, 50, $4)`, uuid.New(), other, tx, now),
			run(`UPDATE wallets SET balance_minor = 50, version = 2, last_transaction_id = $2 WHERE id = $1`, other, tx),
		)
		require.Empty(t, state)
	})
}

// movementFixture builds the statements of a movement on a wallet with 1000 and
// version 2, in pieces so a test can leave one out or get one wrong.
type movementFixture struct {
	s              *schema
	wallet, player uuid.UUID
}

func newMovementFixture(t *testing.T) movementFixture {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	s.credit(t, wallet, player, 0, 1000, 2)
	return movementFixture{s: s, wallet: wallet, player: player}
}

func (f movementFixture) newTx(id uuid.UUID) func(pgx.Tx) error {
	return run(`INSERT INTO wager_transactions (id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
		VALUES ($1, 'EXTERNAL', 'BET', 'p', $1::uuid::text, $1::uuid::text, 'h', $2, $2, $3, 300, 'BRL', 'PROCESSED', $4, $4)`, id, f.wallet, f.player, now)
}

func (f movementFixture) entry(id uuid.UUID, before, after int64) func(pgx.Tx) error {
	return run(`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
		VALUES ($1, $2, $3, 'DEBIT', $4, $5, $6, $7)`, uuid.New(), f.wallet, id, before-after, before, after, now)
}

func (f movementFixture) move(id uuid.UUID, balance, version int64) func(pgx.Tx) error {
	return run(`UPDATE wallets SET balance_minor = $2, version = $3, last_transaction_id = $4 WHERE id = $1`, f.wallet, balance, version, id)
}

// The wallet side of the coherence check: a balance that changed must be explained by
// an entry that starts where the wallet was and ends where it is. Alone, with the
// ledger side turned off.
func TestAWalletsBalanceChangeNeedsItsMatchingEntry(t *testing.T) {
	f := newMovementFixture(t)
	f.s.disable(t, "wallet_ledger_entries", "wallet_ledger_entries_wallet_guard")

	t.Run("no entry at all", func(t *testing.T) {
		id := uuid.New()
		require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.move(id, 700, 3)))
	})
	t.Run("an entry that does not start from the wallet's balance", func(t *testing.T) {
		id := uuid.New()
		require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.entry(id, 900, 600), f.move(id, 600, 3)),
			"balance_before 900 while the wallet had 1000")
	})
	t.Run("an entry that does not end at the wallet's new balance", func(t *testing.T) {
		id := uuid.New()
		require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.entry(id, 1000, 700), f.move(id, 650, 3)))
	})
	t.Run("an entry for another transaction than the wallet's last", func(t *testing.T) {
		id, other := uuid.New(), uuid.New()
		require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.newTx(other), f.entry(id, 1000, 700), f.move(other, 700, 3)))
	})
}

// The ledger side: an entry needs the balance change it describes. Alone, with the
// wallet side turned off.
func TestAnEntryNeedsTheBalanceChangeItDescribes(t *testing.T) {
	f := newMovementFixture(t)
	f.s.disable(t, "wallets", "wallets_ledger_guard")

	t.Run("an entry without any balance change", func(t *testing.T) {
		id := uuid.New()
		require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.entry(id, 1000, 700)))
	})
	t.Run("an entry that does not end at the wallet's balance", func(t *testing.T) {
		id := uuid.New()
		require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.entry(id, 1000, 700), f.move(id, 650, 3)))
	})
	t.Run("an entry for another transaction than the wallet's last", func(t *testing.T) {
		id, other := uuid.New(), uuid.New()
		require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.newTx(other), f.entry(id, 1000, 700), f.move(other, 700, 3)))
	})
}

// With both sides standing: correct movements commit in either order, anything else
// aborts the whole database transaction and leaves nothing behind.
func TestBalanceAndLedgerMustAgreeAtCommit(t *testing.T) {
	f := newMovementFixture(t)

	id := uuid.New()
	require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.move(id, 700, 3)), "a balance without its entry")
	id = uuid.New()
	require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.entry(id, 1000, 700)), "an entry without its balance")
	id = uuid.New()
	require.Equal(t, checkViolation, f.s.inTx(t, f.s.owner, f.newTx(id), f.entry(id, 900, 600), f.move(id, 600, 3)), "a chain that does not chain")

	id = uuid.New()
	require.Empty(t, f.s.inTx(t, f.s.owner, f.newTx(id), f.move(id, 700, 3), f.entry(id, 1000, 700)), "wallet before entry")
	id = uuid.New()
	require.Empty(t, f.s.inTx(t, f.s.owner, f.newTx(id), f.entry(id, 700, 400), f.move(id, 400, 4)), "entry before wallet")

	var balance, version int64
	require.NoError(t, f.s.owner.QueryRow(f.s.ctx, `SELECT balance_minor, version FROM wallets WHERE id = $1`, f.wallet).Scan(&balance, &version))
	require.Equal(t, int64(400), balance)
	require.Equal(t, int64(4), version)
	var entries int
	require.NoError(t, f.s.owner.QueryRow(f.s.ctx, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, f.wallet).Scan(&entries))
	require.Equal(t, 3, entries, "the opening credit and the two correct movements, nothing from the refused ones")
}

func TestOpeningAWalletWithABalanceNeedsItsEntryToo(t *testing.T) {
	s := newSchema(t)
	player := uuid.New()
	openWith := func(balance int64, withEntry bool) string {
		wallet, opening := uuid.New(), uuid.New()
		statements := []func(pgx.Tx) error{
			run(`INSERT INTO wallets (id, player_id, currency, balance_minor, version, last_transaction_id, created_at, updated_at)
				VALUES ($1, $2, 'BRL', $3, 1, $4, $5, $5)`, wallet, uuid.New(), balance, nilIf(balance == 0, opening), now),
		}
		if balance > 0 {
			statements = append(statements, run(`INSERT INTO wager_transactions (id, origin, kind, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
				VALUES ($1, 'INTERNAL', 'OPENING', $2, $2, $3, $4, 'BRL', 'PROCESSED', $5, $5)`, opening, wallet, player, balance, now))
		}
		if withEntry {
			statements = append(statements, run(`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
				VALUES ($1, $2, $3, 'CREDIT', $4, 0, $4, $5)`, uuid.New(), wallet, opening, balance, now))
		}
		return s.inTx(t, s.owner, statements...)
	}
	require.Empty(t, openWith(0, false), "an empty wallet needs no entry")
	require.Empty(t, openWith(5000, true))
	require.Equal(t, checkViolation, openWith(5000, false), "a funded wallet without the credit that funded it")
}

func nilIf(condition bool, id uuid.UUID) any {
	if condition {
		return nil
	}
	return id
}

// --- outbox ------------------------------------------------------------------------

func TestOutboxConstraintsAndIndex(t *testing.T) {
	s := newSchema(t)
	insert := func(eventType string, version int, payload string) string {
		return s.exec(s.owner, `INSERT INTO outbox (event_id, aggregate_id, event_type, version, payload, occurred_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6)`, uuid.New(), uuid.New(), eventType, version, payload, now)
	}
	require.Empty(t, insert("WalletBalanceChanged", 1, `{"a":1}`))
	require.Equal(t, checkViolation, insert("", 1, `{}`), "an event has a type")
	require.Equal(t, checkViolation, insert("X", 0, `{}`), "versions start at 1")
	require.Equal(t, checkViolation, insert("X", 1, `[1,2]`), "the payload is an object")
	require.Equal(t, checkViolation, insert("X", 1, `"text"`), "the payload is an object")

	var definition string
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT indexdef FROM pg_indexes WHERE tablename = 'outbox' AND indexname = 'outbox_unpublished'`).Scan(&definition))
	require.Contains(t, definition, "WHERE (published_at IS NULL)", "only unpublished events are indexed")
	require.Contains(t, definition, "occurred_at")
}

// --- outbox publishing state -------------------------------------------------------

func (s *schema) outboxEvent(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.Empty(t, s.exec(s.owner, `INSERT INTO outbox (event_id, aggregate_id, event_type, version, payload, correlation_id, causation_id, occurred_at)
		VALUES ($1, $2, 'WalletBalanceChanged', 1, '{"a":1}', 'corr', 'cause', $3)`, id, uuid.New(), now))
	return id
}

func TestOutboxDefaultsAndOrderKey(t *testing.T) {
	s := newSchema(t)
	first, second := s.outboxEvent(t), s.outboxEvent(t)

	var attempts int
	var lockedUntil, publishedAt *time.Time
	var nextAttempt time.Time
	var seq1, seq2 int64
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT attempts, next_attempt_at, locked_until, published_at, seq FROM outbox WHERE event_id = $1`, first).
		Scan(&attempts, &nextAttempt, &lockedUntil, &publishedAt, &seq1))
	require.Zero(t, attempts)
	require.Nil(t, lockedUntil)
	require.Nil(t, publishedAt)
	require.WithinDuration(t, time.Now(), nextAttempt, time.Minute, "due immediately")
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT seq FROM outbox WHERE event_id = $1`, second).Scan(&seq2))
	require.Greater(t, seq2, seq1, "events are ordered by insertion")

	require.NotEmpty(t, s.exec(s.owner, `INSERT INTO outbox (event_id, aggregate_id, event_type, version, payload, occurred_at, seq)
		VALUES (gen_random_uuid(), gen_random_uuid(), 'X', 1, '{}', now(), 1)`), "seq is assigned by the database, never by the writer")
	require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE outbox SET attempts = -1 WHERE event_id = $1`, first))
}

func TestOutboxContentIsImmutableAndOnlyPublishingStateMoves(t *testing.T) {
	s := newSchema(t)
	id := s.outboxEvent(t)

	for _, change := range []string{
		`payload = '{"a":2}'`, `event_type = 'Other'`, `version = 2`, `aggregate_id = gen_random_uuid()`,
		`correlation_id = 'other'`, `causation_id = NULL`, `occurred_at = now()`, `event_id = gen_random_uuid()`,
	} {
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE outbox SET `+change+` WHERE event_id = $1`, id), change)
	}

	require.Equal(t, "428C9", s.exec(s.owner, `UPDATE outbox SET seq = seq + 100 WHERE event_id = $1`, id), "the order key is generated, not editable")
	require.Empty(t, s.exec(s.owner, `UPDATE outbox SET attempts = attempts + 1, next_attempt_at = now() + interval '1 minute',
		locked_until = now() + interval '30 seconds', last_error = 'boom' WHERE event_id = $1`, id), "the publishing state moves")
	require.Empty(t, s.exec(s.owner, `UPDATE outbox SET locked_until = NULL WHERE event_id = $1`, id))
	require.Empty(t, s.exec(s.owner, `UPDATE outbox SET published_at = now() WHERE event_id = $1`, id))

	t.Run("a published event never changes again", func(t *testing.T) {
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE outbox SET published_at = NULL WHERE event_id = $1`, id), "no un-publishing")
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE outbox SET attempts = attempts + 1 WHERE event_id = $1`, id))
		require.Equal(t, checkViolation, s.exec(s.owner, `UPDATE outbox SET published_at = now() WHERE event_id = $1`, id), "nor a second publication time")
	})
}

func TestTheApplicationRoleCanPublishButNotRewrite(t *testing.T) {
	s := newSchema(t)
	id := s.outboxEvent(t)
	require.Empty(t, s.exec(s.app, `UPDATE outbox SET attempts = attempts + 1, last_error = 'x' WHERE event_id = $1`, id))
	require.Equal(t, checkViolation, s.exec(s.app, `UPDATE outbox SET payload = '{}' WHERE event_id = $1`, id))
	var n int
	require.NoError(t, s.app.QueryRow(s.ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestTheOutboxIndexesServeThePublisher(t *testing.T) {
	s := newSchema(t)
	for _, c := range []struct{ name, contains string }{
		{"outbox_due", "(seq)"},
		{"outbox_unpublished_by_wallet", "(aggregate_id, seq)"},
	} {
		var definition string
		require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT indexdef FROM pg_indexes WHERE tablename = 'outbox' AND indexname = $1`, c.name).Scan(&definition))
		require.Contains(t, definition, c.contains, c.name)
		require.Contains(t, definition, "WHERE (published_at IS NULL)", c.name)
	}
}

// --- the ledger's order key --------------------------------------------------------

func TestLedgerEntriesGetAnOrderKeyFromTheDatabase(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	first := s.credit(t, wallet, player, 0, 1000, 2)
	second := s.credit(t, wallet, player, 1000, 500, 3)

	var seq1, seq2 int64
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT seq FROM wallet_ledger_entries WHERE transaction_id = $1`, first).Scan(&seq1))
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT seq FROM wallet_ledger_entries WHERE transaction_id = $1`, second).Scan(&seq2))
	require.Greater(t, seq2, seq1, "a later entry has a higher key")

	require.NotEmpty(t, s.exec(s.owner, `INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at, seq)
		VALUES (gen_random_uuid(), $1, $2, 'CREDIT', 1, 0, 1, now(), 1)`, wallet, first), "the key is the database's to give")
	require.Equal(t, "428C9", s.exec(s.owner, `UPDATE wallet_ledger_entries SET seq = seq + 1`), "or to change")
}

func TestTheLedgerOrderKeyIsUnique(t *testing.T) {
	s := newSchema(t)
	wallet, player := s.wallet(t)
	s.credit(t, wallet, player, 0, 1000, 2)
	// Identity columns do not enforce uniqueness by themselves: the constraint does.
	var constraint string
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT conname FROM pg_constraint WHERE conrelid = 'wallet_ledger_entries'::regclass AND contype = 'u' AND conname = 'wallet_ledger_entries_seq_key'`).Scan(&constraint))
	var index string
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT indexdef FROM pg_indexes WHERE tablename = 'wallet_ledger_entries' AND indexname = 'wallet_ledger_entries_wallet_seq'`).Scan(&index))
	require.Contains(t, index, "(wallet_id, seq)")
}
