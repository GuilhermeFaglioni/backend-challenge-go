//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type reconciliationJSON struct {
	WalletID          string                  `json:"walletId"`
	StoredBalance     struct{ Amount string } `json:"storedBalance"`
	CalculatedBalance struct{ Amount string } `json:"calculatedBalance"`
	Difference        struct{ Amount string } `json:"difference"`
	Consistent        bool                    `json:"consistent"`
	CheckedEntries    int64                   `json:"checkedEntries"`
	Error             struct{ Code string }   `json:"error"`
}

func (a walletAPI) reconcile(t *testing.T, client *contractClient, token, walletID string) (int, reconciliationJSON) {
	t.Helper()
	status, _, raw := client.call(t.Context(), http.MethodPost, a.base+"/wallets/"+walletID+"/reconciliation", token, "")
	var result reconciliationJSON
	_ = json.Unmarshal([]byte(raw), &result)
	return status, result
}

// fingerprint hashes every row of the tables, system columns included, so that any write at
// all, even one that changes nothing, shows.
func (a walletAPI) fingerprint(t *testing.T) string {
	t.Helper()
	var sum string
	require.NoError(t, a.pool.QueryRow(t.Context(), `
		SELECT md5(
			(SELECT coalesce(string_agg(w.xmin::text || w::text, '|' ORDER BY w.id), '') FROM wallets w) ||
			(SELECT coalesce(string_agg(l.xmin::text || l::text, '|' ORDER BY l.id), '') FROM wallet_ledger_entries l) ||
			(SELECT coalesce(string_agg(t.xmin::text || t::text, '|' ORDER BY t.id), '') FROM wager_transactions t) ||
			(SELECT coalesce(string_agg(o.xmin::text || o::text, '|' ORDER BY o.event_id), '') FROM outbox o))`).Scan(&sum))
	return sum
}

func TestAConsistentWalletIsReconciledAndNothingIsChanged(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	// Another wallet, with a different history: its entries must not leak into w's books.
	other := a.openFunded(t, "40.00")
	status, _, _ := a.submit(t, a.strict, a.provider, "other-1", operation(other, "other-tx-1", "BET", "3.00"))
	require.Equal(t, http.StatusOK, status)
	for i := 1; i <= 5; i++ {
		status, _, _ := a.submit(t, a.strict, a.provider, fmt.Sprintf("k-%d", i), operation(w, fmt.Sprintf("tx-%d", i), "BET", "7.00"))
		require.Equal(t, http.StatusOK, status)
	}
	// Nothing else may be writing for the fingerprint to mean anything: the publisher of
	// the application marks outbox rows, so wait until it has finished.
	require.Eventually(t, func() bool {
		var pending int
		_ = a.pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&pending)
		return pending == 0
	}, eventsWithin, 50*time.Millisecond)
	before := a.fingerprint(t)

	_, otherResult := a.reconcile(t, a.strict, a.internal, other.id)
	require.True(t, otherResult.Consistent)
	require.Equal(t, "37.00", otherResult.StoredBalance.Amount)
	require.Equal(t, int64(2), otherResult.CheckedEntries)

	status, result := a.reconcile(t, a.strict, a.internal, w.id)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, w.id, result.WalletID)
	require.True(t, result.Consistent)
	require.Equal(t, "65.00", result.StoredBalance.Amount)
	require.Equal(t, "65.00", result.CalculatedBalance.Amount)
	require.Equal(t, "0.00", result.Difference.Amount)
	require.Equal(t, int64(6), result.CheckedEntries, "the opening and five bets")

	for i := 0; i < 5; i++ {
		_, again := a.reconcile(t, a.strict, a.internal, w.id)
		require.True(t, again.Consistent)
	}
	require.Equal(t, before, a.fingerprint(t), "reconciling changes nothing: not a row, not even a row version")
	a.strict.requireNoViolations()
}

// The books are made to disagree the only way they can be, by the owner of the schema
// switching off the triggers that keep them in step.
func TestAForcedDivergenceIsReportedLoggedAndCounted(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	status, _, _ := a.submit(t, a.strict, a.provider, "k-1", operation(w, "tx-1", "BET", "25.00"))
	require.Equal(t, http.StatusOK, status)

	owner := connect(t.Context(), t, a.ownerDSN)
	_, err := owner.Exec(t.Context(), `ALTER TABLE wallets DISABLE TRIGGER USER`)
	require.NoError(t, err)

	t.Run("more stored than the ledger explains", func(t *testing.T) {
		_, err := owner.Exec(t.Context(), `UPDATE wallets SET balance_minor = balance_minor + 500 WHERE id = $1`, w.id)
		require.NoError(t, err)

		status, result := a.reconcile(t, a.strict, a.internal, w.id)
		require.Equal(t, http.StatusOK, status, "a divergence is a finding, not an error of the request")
		require.False(t, result.Consistent)
		require.Equal(t, "80.00", result.StoredBalance.Amount)
		require.Equal(t, "75.00", result.CalculatedBalance.Amount)
		require.Equal(t, "5.00", result.Difference.Amount)

		logged := a.logs.String()
		require.Contains(t, logged, "reconciliation found a divergence")
		var found bool
		for _, line := range strings.Split(logged, "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "reconciliation found a divergence" {
				found = true
				require.Equal(t, "ERROR", entry["level"])
				require.Equal(t, w.id, entry["walletId"])
				require.Equal(t, "5.00", entry["difference"])
			}
		}
		require.True(t, found)
	})

	t.Run("less stored than the ledger explains", func(t *testing.T) {
		_, err := owner.Exec(t.Context(), `UPDATE wallets SET balance_minor = 7200 WHERE id = $1`, w.id)
		require.NoError(t, err)
		status, result := a.reconcile(t, a.strict, a.internal, w.id)
		require.Equal(t, http.StatusOK, status)
		require.False(t, result.Consistent)
		require.Equal(t, "-3.00", result.Difference.Amount, "stored minus calculated, so negative")
	})

	t.Run("repaired, it is consistent again", func(t *testing.T) {
		_, err := owner.Exec(t.Context(), `UPDATE wallets SET balance_minor = 7500 WHERE id = $1`, w.id)
		require.NoError(t, err)
		_, result := a.reconcile(t, a.strict, a.internal, w.id)
		require.True(t, result.Consistent)
	})

	resp, err := http.Get(a.base + "/metrics") //nolint:noctx // a test fixture request
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	metrics, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(metrics), "reconciliation_divergences_total 2", "the two divergent checks, not the healthy one")
	require.Contains(t, string(metrics), `reconciliations_total{result="divergent"} 2`)
	require.Contains(t, string(metrics), `reconciliations_total{result="consistent"} 1`)
	a.strict.requireNoViolations()
}

// While bets are being committed on the wallet, every reconciliation sees books that
// agree: the stored balance and the ledger are read from the same snapshot. Read in two
// statements of their own, some of them would catch a bet half way.
func TestReconcilingWhileTheWalletIsBeingWrittenIsAlwaysConsistent(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "10000.00")

	const writers, betsPerWriter = 4, 40
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < betsPerWriter; i++ {
				status, _, _ := a.submit(t, a.client, a.provider, fmt.Sprintf("w%d-%d", writer, i), operation(w, fmt.Sprintf("w%d-tx-%d", writer, i), "BET", "1.00"))
				if status != http.StatusOK {
					t.Errorf("writer %d bet %d: %d", writer, i, status)
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	var checks, divergent atomic.Int64
	var lastEntries int64
	var readers sync.WaitGroup
	for reader := 0; reader < 3; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				status, result := a.reconcile(t, a.client, a.internal, w.id)
				if status != http.StatusOK {
					t.Errorf("reconciliation answered %d", status)
					return
				}
				checks.Add(1)
				if !result.Consistent {
					divergent.Add(1)
					t.Errorf("books apart under concurrent writes: stored %s, calculated %s", result.StoredBalance.Amount, result.CalculatedBalance.Amount)
				}
				_ = lastEntries
			}
		}()
	}
	readers.Wait()

	require.Zero(t, divergent.Load())
	require.Greater(t, checks.Load(), int64(10), "the checks must overlap the writes to prove anything")
	t.Logf("%d reconciliations overlapped %d bets", checks.Load(), writers*betsPerWriter)

	status, result := a.reconcile(t, a.client, a.internal, w.id)
	require.Equal(t, http.StatusOK, status)
	require.True(t, result.Consistent)
	require.Equal(t, int64(1+writers*betsPerWriter), result.CheckedEntries)
	require.Equal(t, fmt.Sprintf("%d.00", 10000-writers*betsPerWriter), result.StoredBalance.Amount)
}

func TestReconciliationRefusesWhatItShouldAndTouchesNothing(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "10.00")
	before := a.tables(t)

	status, body := a.reconcile(t, a.client, a.internal, uuid.NewString())
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "NOT_FOUND", body.Error.Code)
	status, _ = a.reconcile(t, a.client, a.internal, "not-a-uuid")
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = a.reconcile(t, a.client, a.provider, w.id)
	require.Equal(t, http.StatusForbidden, status, "providers do not reconcile wallets")
	status, _ = a.reconcile(t, a.client, "", w.id)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, before, a.tables(t))
	a.client.requireNoViolations()
}
