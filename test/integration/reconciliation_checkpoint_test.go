//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type checkpointRow struct {
	ThroughSeq      int64
	Credits, Debits string
	Entries         int64
}

func (a walletAPI) checkpoints(t *testing.T, walletID string) []checkpointRow {
	t.Helper()
	rows, err := a.pool.Query(t.Context(), `
		SELECT through_seq, credits_minor::text, debits_minor::text, entries
		FROM reconciliation_checkpoints WHERE wallet_id = $1 ORDER BY through_seq`, walletID)
	require.NoError(t, err)
	defer rows.Close()
	var out []checkpointRow
	for rows.Next() {
		var c checkpointRow
		require.NoError(t, rows.Scan(&c.ThroughSeq, &c.Credits, &c.Debits, &c.Entries))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

func (a walletAPI) lastSeq(t *testing.T, walletID string) int64 {
	t.Helper()
	var seq int64
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT max(seq) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&seq))
	return seq
}

func (a walletAPI) bets(t *testing.T, w apiWallet, prefix string, n int, amount string) {
	t.Helper()
	for i := 1; i <= n; i++ {
		status, _, body := a.submit(t, a.strict, a.provider, fmt.Sprintf("%s-k-%d", prefix, i), operation(w, fmt.Sprintf("%s-tx-%d", prefix, i), "BET", amount))
		require.Equal(t, http.StatusOK, status, body)
	}
}

// A consistent check leaves a checkpoint, and the next one reads only what came after it:
// the cost of a reconciliation follows what happened since the last one, not the size of
// the ledger.
func TestAReconciliationContinuesFromTheLastCheckpoint(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	a.bets(t, w, "first", 3, "5.00")

	status, first := a.reconcile(t, a.strict, a.internal, w.id)
	require.Equal(t, http.StatusOK, status)
	require.True(t, first.Consistent)
	require.Equal(t, "incremental", first.Mode)
	require.Equal(t, int64(4), first.ReadEntries, "no checkpoint yet: the opening and three bets")
	require.Equal(t, int64(4), first.CheckedEntries)
	require.Nil(t, first.CheckpointMatches)
	require.Equal(t, []checkpointRow{{ThroughSeq: a.lastSeq(t, w.id), Credits: "10000", Debits: "1500", Entries: 4}}, a.checkpoints(t, w.id),
		"the database computed the checkpoint from the ledger")

	a.bets(t, w, "second", 2, "10.00")
	_, second := a.reconcile(t, a.strict, a.internal, w.id)
	require.True(t, second.Consistent)
	require.Equal(t, "65.00", second.CalculatedBalance.Amount)
	require.Equal(t, int64(2), second.ReadEntries, "only the two bets after the checkpoint")
	require.Equal(t, int64(6), second.CheckedEntries)
	require.Len(t, a.checkpoints(t, w.id), 2)
	require.Equal(t, checkpointRow{ThroughSeq: a.lastSeq(t, w.id), Credits: "10000", Debits: "3500", Entries: 6}, a.checkpoints(t, w.id)[1])

	_, third := a.reconcile(t, a.strict, a.internal, w.id)
	require.True(t, third.Consistent)
	require.Zero(t, third.ReadEntries, "nothing new to read")
	require.Equal(t, int64(6), third.CheckedEntries)
	require.Len(t, a.checkpoints(t, w.id), 2, "nothing new, no new checkpoint")

	// The stored balance is still checked against checkpoint plus tail.
	owner := connect(t.Context(), t, a.ownerDSN)
	_, err := owner.Exec(t.Context(), `ALTER TABLE wallets DISABLE TRIGGER USER`)
	require.NoError(t, err)
	_, err = owner.Exec(t.Context(), `UPDATE wallets SET balance_minor = balance_minor + 1 WHERE id = $1`, w.id)
	require.NoError(t, err)
	_, corrupted := a.reconcile(t, a.strict, a.internal, w.id)
	require.False(t, corrupted.Consistent)
	require.Equal(t, "0.01", corrupted.Difference.Amount)
	a.strict.requireNoViolations()
}

// A full check reads the whole ledger and holds the last checkpoint against it: a
// checkpoint that does not summarise the ledger is a finding, even when the balance
// matches.
func TestAFullReconciliationRereadsTheLedgerAndChecksTheCheckpoint(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	a.bets(t, w, "b", 2, "5.00")
	_, first := a.reconcile(t, a.strict, a.internal, w.id)
	require.True(t, first.Consistent)
	// A debit and a credit after the checkpoint, so that the check of the checkpoint must
	// stop at its seq on both sides.
	a.bets(t, w, "c", 1, "5.00")
	a.mustProcess(t, w, "win-1", "WIN", "7.00", "")

	_, full := a.reconcileFull(t, a.strict, a.internal, w.id)
	require.True(t, full.Consistent)
	require.Equal(t, "full", full.Mode)
	require.Equal(t, "92.00", full.CalculatedBalance.Amount)
	require.Equal(t, int64(5), full.ReadEntries, "everything, the checkpoint notwithstanding")
	require.Equal(t, int64(5), full.CheckedEntries)
	require.NotNil(t, full.CheckpointMatches)
	require.True(t, *full.CheckpointMatches)
	require.Len(t, a.checkpoints(t, w.id), 2, "a consistent full check records one too")

	// Checkpoints are append-only even for the owner, who has to switch the triggers off to
	// forge one: the last checkpoint is made to claim 5.00 more credits than the ledger has.
	owner := connect(t.Context(), t, a.ownerDSN)
	_, err := owner.Exec(t.Context(), `ALTER TABLE reconciliation_checkpoints DISABLE TRIGGER USER`)
	require.NoError(t, err)
	_, err = owner.Exec(t.Context(), `UPDATE reconciliation_checkpoints SET credits_minor = credits_minor + 500
		WHERE wallet_id = $1 AND through_seq = (SELECT max(through_seq) FROM reconciliation_checkpoints WHERE wallet_id = $1)`, w.id)
	require.NoError(t, err)
	_, err = owner.Exec(t.Context(), `ALTER TABLE reconciliation_checkpoints ENABLE TRIGGER USER`)
	require.NoError(t, err)

	_, incremental := a.reconcile(t, a.strict, a.internal, w.id)
	require.False(t, incremental.Consistent, "the incremental check trusts the checkpoint, and the balance no longer agrees with it")
	require.Equal(t, "-5.00", incremental.Difference.Amount)

	_, audit := a.reconcileFull(t, a.strict, a.internal, w.id)
	require.False(t, audit.Consistent)
	require.Equal(t, "0.00", audit.Difference.Amount, "the balance agrees with the ledger")
	require.NotNil(t, audit.CheckpointMatches)
	require.False(t, *audit.CheckpointMatches, "but the checkpoint does not")
	require.Contains(t, a.logs.String(), "reconciliation found a checkpoint that does not match the ledger")
	a.strict.requireNoViolations()
}

// The function computes a checkpoint from the ledger and refuses what it cannot vouch for.
func TestTheCheckpointFunctionRefusesWhatItCannotVouchFor(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	other := a.openFunded(t, "50.00")
	a.bets(t, w, "b", 2, "5.00")

	app := a.pool // the application role
	var recorded bool

	t.Run("a seq of another wallet", func(t *testing.T) {
		err := app.QueryRow(t.Context(), `SELECT record_reconciliation_checkpoint($1, $2)`, w.id, a.lastSeq(t, other.id)).Scan(&recorded)
		require.Error(t, err)
		require.Equal(t, "23514", sqlState(err), "check_violation: %v", err)
	})
	t.Run("a checkpoint through the last entry, then an earlier one", func(t *testing.T) {
		require.NoError(t, app.QueryRow(t.Context(), `SELECT record_reconciliation_checkpoint($1, $2)`, w.id, a.lastSeq(t, w.id)).Scan(&recorded))
		require.True(t, recorded)
		require.NoError(t, app.QueryRow(t.Context(), `SELECT record_reconciliation_checkpoint($1, $2)`, w.id, a.lastSeq(t, w.id)).Scan(&recorded))
		require.False(t, recorded, "already there")
		require.NoError(t, app.QueryRow(t.Context(), `SELECT record_reconciliation_checkpoint($1, $2)`, w.id, a.lastSeq(t, w.id)-1).Scan(&recorded))
		require.False(t, recorded, "behind the last checkpoint")
		require.Equal(t, []checkpointRow{{ThroughSeq: a.lastSeq(t, w.id), Credits: "10000", Debits: "1000", Entries: 3}}, a.checkpoints(t, w.id))
	})
	t.Run("a ledger that does not add up", func(t *testing.T) {
		// The owner slips in an entry whose arithmetic holds on its own but that does not
		// continue from the balance before it.
		owner := connect(t.Context(), t, a.ownerDSN)
		_, err := owner.Exec(t.Context(), `ALTER TABLE wallet_ledger_entries DISABLE TRIGGER USER`)
		require.NoError(t, err)
		_, err = owner.Exec(t.Context(), `
			INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, amount_minor, balance_before_minor, balance_after_minor, created_at)
			SELECT gen_random_uuid(), $1, transaction_id, 'CREDIT', 100, 50000, 50100, now()
			FROM wallet_ledger_entries WHERE wallet_id = $2 LIMIT 1`, w.id, other.id)
		require.NoError(t, err)
		err = app.QueryRow(t.Context(), `SELECT record_reconciliation_checkpoint($1, $2)`, w.id, a.lastSeq(t, w.id)).Scan(&recorded)
		require.Error(t, err)
		require.Equal(t, "23514", sqlState(err), "check_violation: %v", err)
		require.Len(t, a.checkpoints(t, w.id), 1, "nothing recorded")
	})
}
