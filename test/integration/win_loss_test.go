//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

func (a walletAPI) wallet(t *testing.T, w apiWallet) (balance, version int64) {
	t.Helper()
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT balance_minor, version FROM wallets WHERE id = $1`, w.id).Scan(&balance, &version))
	return balance, version
}

func (a walletAPI) eventTypes(t *testing.T, w apiWallet, transactionID string) []string {
	t.Helper()
	rows, err := a.pool.Query(t.Context(), `SELECT event_type FROM outbox WHERE aggregate_id = $1 AND payload->>'transactionId' = $2 ORDER BY seq`, w.id, transactionID)
	require.NoError(t, err)
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ string
		require.NoError(t, rows.Scan(&typ))
		types = append(types, typ)
	}
	require.NoError(t, rows.Err())
	return types
}

func (a walletAPI) requireConsistent(t *testing.T, w apiWallet) {
	t.Helper()
	status, result := a.reconcile(t, a.client, a.internal, w.id)
	require.Equal(t, http.StatusOK, status)
	require.True(t, result.Consistent, "stored %s, calculated %s", result.StoredBalance.Amount, result.CalculatedBalance.Amount)
}

func TestAWinCreditsAndALossMovesNothingAndBothFollowTheContract(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")

	status, _, bet := a.submit(t, a.strict, a.provider, "k-bet", operation(w, "tx-bet", "BET", "30.00"))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "70.00", bet.Balance.Amount)

	status, _, win := a.submit(t, a.strict, a.provider, "k-win", operation(w, "tx-win", "WIN", "55.50"))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "PROCESSED", win.Status)
	require.Equal(t, "125.50", win.Balance.Amount)
	balance, version := a.wallet(t, w)
	require.Equal(t, int64(12550), balance)
	require.Equal(t, int64(3), version, "opening, bet, win")

	status, _, loss := a.submit(t, a.strict, a.provider, "k-loss", operation(w, "tx-loss", "LOSS", "0.00"))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "PROCESSED", loss.Status)
	require.Equal(t, "125.50", loss.Balance.Amount, "the balance as it stands")
	balance, version = a.wallet(t, w)
	require.Equal(t, int64(12550), balance)
	require.Equal(t, int64(3), version, "a loss does not change the wallet's version")

	// The ledger has the opening, the debit and the credit, and nothing for the loss.
	entries := a.walk(t, a.strict, w.id, 50)
	var directions []string
	for _, e := range entries {
		directions = append(directions, e.Direction)
	}
	require.Equal(t, []string{"CREDIT", "DEBIT", "CREDIT"}, directions)
	require.Equal(t, win.TransactionID, entries[2].TransactionID)
	require.Equal(t, "55.50", entries[2].Money.Amount)
	require.Equal(t, "70.00", entries[2].BalanceBefore.Amount)
	require.Equal(t, "125.50", entries[2].BalanceAfter.Amount)
	var lossEntries int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries WHERE transaction_id = $1`, loss.TransactionID).Scan(&lossEntries))
	require.Zero(t, lossEntries)

	// The events: the win like a bet, the loss without a balance change.
	require.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, a.eventTypes(t, w, win.TransactionID))
	require.Equal(t, []string{"WagerTransactionProcessed"}, a.eventTypes(t, w, loss.TransactionID))

	// Reads of both.
	for _, id := range []string{win.TransactionID, loss.TransactionID} {
		status, _, _, tx := a.read(t, a.strict, a.provider, "/wagering/transactions/"+id)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "PROCESSED", tx.Status)
		require.Equal(t, "125.50", tx.Balance.Amount)
	}
	_, _, _, tx := a.read(t, a.strict, a.provider, "/providers/provider-a/wagering/transactions/tx-loss")
	require.Equal(t, "LOSS", tx.Kind)

	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestWinsAndLossesAreReplayedWithTheirOriginalBalance(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	_, _, win := a.submit(t, a.strict, a.provider, "k-win", operation(w, "tx-win", "WIN", "10.00"))
	_, _, loss := a.submit(t, a.strict, a.provider, "k-loss", operation(w, "tx-loss", "LOSS", "0.00"))
	status, _, _ := a.submit(t, a.strict, a.provider, "k-bet", operation(w, "tx-bet", "BET", "60.00"))
	require.Equal(t, http.StatusOK, status)
	before := a.tables(t)

	status, _, again := a.submit(t, a.strict, a.provider, "k-win", operation(w, "tx-win", "WIN", "10.00"))
	require.Equal(t, http.StatusOK, status)
	require.True(t, again.IdempotentReplay)
	require.Equal(t, win.TransactionID, again.TransactionID)
	require.Equal(t, "110.00", again.Balance.Amount, "the balance of the first time, not the current one")

	status, _, again = a.submit(t, a.strict, a.provider, "k-loss", operation(w, "tx-loss", "LOSS", "0.00"))
	require.Equal(t, http.StatusOK, status)
	require.True(t, again.IdempotentReplay)
	require.Equal(t, loss.TransactionID, again.TransactionID)
	require.Equal(t, "110.00", again.Balance.Amount)

	require.Equal(t, before, a.tables(t), "nothing written")
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestConflictsAndAuthorizationAreTheSameAsForABet(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	status, _, _ := a.submit(t, a.strict, a.provider, "k-win", operation(w, "tx-win", "WIN", "10.00"))
	require.Equal(t, http.StatusOK, status)
	before := a.tables(t)

	for name, c := range map[string]struct {
		key, body string
		want      int
		code      string
	}{
		"the key with another amount":  {"k-win", operation(w, "tx-win", "WIN", "11.00"), http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT"},
		"the key as another kind":      {"k-win", operation(w, "tx-win", "BET", "10.00"), http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT"},
		"the external id, other key":   {"k-other", operation(w, "tx-win", "WIN", "10.00"), http.StatusConflict, "DUPLICATE_EXTERNAL_TRANSACTION"},
		"a loss reusing the win's key": {"k-win", operation(w, "tx-win", "LOSS", "0.00"), http.StatusConflict, "IDEMPOTENCY_KEY_CONFLICT"},
	} {
		status, _, answer := a.submit(t, a.strict, a.provider, c.key, c.body)
		require.Equal(t, c.want, status, name)
		require.Equal(t, c.code, answer.Error.Code, name)
	}
	for name, token := range map[string]string{"another provider": a.providerB, "the internal role": a.internal, "no token": ""} {
		want := http.StatusForbidden
		if token == "" {
			want = http.StatusUnauthorized
		}
		for _, kind := range []struct{ kind, amount string }{{"WIN", "10.00"}, {"LOSS", "0.00"}} {
			status, _, _ := a.submit(t, a.strict, token, "k-x", operation(w, "tx-x", kind.kind, kind.amount))
			require.Equal(t, want, status, name+" "+kind.kind)
		}
	}
	require.Equal(t, before, a.tables(t), "none of it wrote anything")
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestWinsAndLossesAreRefusedForTheWrongWalletOrPlayerAndStoredAsRejections(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	stranger := apiWallet{id: w.id, player: a.openFunded(t, "1.00").player}

	for _, c := range []struct{ kind, amount, key string }{{"WIN", "5.00", "k1"}, {"LOSS", "0.00", "k2"}} {
		status, _, answer := a.submit(t, a.strict, a.provider, c.key, operation(stranger, "tx-"+c.key, c.kind, c.amount))
		require.Equal(t, http.StatusUnprocessableEntity, status, c.kind)
		require.Equal(t, "PLAYER_MISMATCH", answer.FailureCode, c.kind)
		ghost := apiWallet{id: "3fa85f64-5717-4562-b3fc-2c963f66afa6", player: w.player}
		status, _, answer = a.submit(t, a.strict, a.provider, c.key+"-ghost", operation(ghost, "tx-"+c.key+"-ghost", c.kind, c.amount))
		require.Equal(t, http.StatusUnprocessableEntity, status, c.kind)
		require.Equal(t, "WALLET_NOT_FOUND", answer.FailureCode, c.kind)
	}
	balance, version := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	require.Equal(t, int64(1), version)
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestAWinThatWouldOverflowTheBalanceIsAStoredRejection(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	body := operation(w, "tx-big", "WIN", "92233720368547758.07") // the largest amount there is

	status, _, answer := a.submit(t, a.strict, a.provider, "k-big", body)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "REJECTED", answer.Status)
	require.Equal(t, "BALANCE_LIMIT_EXCEEDED", answer.FailureCode)

	balance, version := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	require.Equal(t, int64(1), version)
	status, _, answer = a.submit(t, a.strict, a.provider, "k-big", body)
	require.Equal(t, http.StatusUnprocessableEntity, status, "and it stays rejected")
	require.True(t, answer.IdempotentReplay)
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

// Bets, wins and losses on one wallet at once: the balance and the version are exactly
// what the sum of the processed operations says, and the books agree.
func TestMixedOperationsOnOneWalletAddUp(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "1000.00")

	const rounds = 15
	var wg sync.WaitGroup
	for i := range rounds {
		for _, op := range []struct{ kind, amount string }{{"BET", "10.00"}, {"WIN", "4.00"}, {"LOSS", "0.00"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("%s-%d", op.kind, i)
				status, _, _ := a.submit(t, a.client, a.provider, "key-"+id, operation(w, id, op.kind, op.amount))
				if status != http.StatusOK {
					t.Errorf("%s answered %d", id, status)
				}
			}()
		}
	}
	wg.Wait()

	balance, version := a.wallet(t, w)
	require.Equal(t, int64(100000-rounds*1000+rounds*400), balance)
	require.Equal(t, int64(1+rounds*2), version, "the opening, every bet and every win; no loss")
	a.requireConsistent(t, w)
}

// --- through the queue ----------------------------------------------------------------

func messageFor(kind, id string, w apiWallet, amount string) string {
	return requestBody("msg-"+id, w, "tx-"+id, "provider-a:tx-"+id, kind, amount)
}

func TestWinsAndLossesArriveByMessageWithTheSameResult(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")

	a.send(t, w.id, messageFor("WIN", "win", w, "25.00"))
	a.send(t, w.id, messageFor("LOSS", "loss", w, "0.00"))
	a.send(t, w.id, messageFor("BET", "bet", w, "5.00"))
	a.drained(t)

	require.Equal(t, int64(12000), a.balanceOf(t, w))
	_, version := a.wallet(t, w)
	require.Equal(t, int64(3), version, "opening, win and bet; the loss left it alone")
	require.Equal(t, 0, a.depth(t, a.queues.DLQURL))

	// Sent again by HTTP, it is the same operation: a replay, no second movement.
	status, _, win := a.submit(t, a.strict, a.provider, "provider-a:tx-win", operation(w, "tx-win", "WIN", "25.00"))
	require.Equal(t, http.StatusOK, status)
	require.True(t, win.IdempotentReplay)
	require.Equal(t, "125.00", win.Balance.Amount)
	status, _, loss := a.submit(t, a.strict, a.provider, "provider-a:tx-loss", operation(w, "tx-loss", "LOSS", "0.00"))
	require.Equal(t, http.StatusOK, status)
	require.True(t, loss.IdempotentReplay)
	require.Equal(t, int64(12000), a.balanceOf(t, w))
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestCopiesOfAWinByMessageAndByHTTPCreditOnce(t *testing.T) {
	a := startConsumerAPI(t, func(c *config.Consumer) { c.Workers = 3 })
	w := a.openFunded(t, "100.00")
	body := messageFor("WIN", "win", w, "25.00")

	for range 6 {
		a.send(t, w.id, body)
	}
	for range 3 {
		status, _, _ := a.submit(t, a.client, a.provider, "provider-a:tx-win", operation(w, "tx-win", "WIN", "25.00"))
		require.Equal(t, http.StatusOK, status)
	}
	a.drained(t)

	require.Equal(t, int64(12500), a.balanceOf(t, w))
	var credits int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'`, w.id).Scan(&credits))
	require.Equal(t, 2, credits, "the opening and the win")
	a.requireConsistent(t, w)
}
