//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// operationRef is an operation that names a Reference.
func operationRef(w apiWallet, externalID, kind, amount, reference string) string {
	return strings.Replace(operation(w, externalID, kind, amount), `"roundId"`, fmt.Sprintf(`"referenceExternalTransactionId":%q,"roundId"`, reference), 1)
}

// withRound changes the round of an operation body.
func withRound(body, round string) string {
	return strings.Replace(body, `"roundId":"round-1"`, fmt.Sprintf(`"roundId":%q`, round), 1)
}

func (a walletAPI) send(t *testing.T, client *contractClient, w apiWallet, externalID, kind, amount, reference string) (int, operationAnswer) {
	t.Helper()
	body := operation(w, externalID, kind, amount)
	if reference != "" {
		body = operationRef(w, externalID, kind, amount, reference)
	}
	status, _, answer := a.submit(t, client, a.provider, "provider-a:"+externalID, body)
	return status, answer
}

func (a walletAPI) mustProcess(t *testing.T, w apiWallet, externalID, kind, amount, reference string) operationAnswer {
	t.Helper()
	status, answer := a.send(t, a.strict, w, externalID, kind, amount, reference)
	require.Equal(t, http.StatusOK, status, "%s %s: %+v", kind, externalID, answer)
	require.Equal(t, "PROCESSED", answer.Status)
	return answer
}

func (a walletAPI) requireRefusal(t *testing.T, w apiWallet, externalID, kind, amount, reference, code string) {
	t.Helper()
	status, answer := a.send(t, a.strict, w, externalID, kind, amount, reference)
	require.Equal(t, http.StatusUnprocessableEntity, status, "%s %s: %+v", kind, externalID, answer)
	require.Equal(t, "REJECTED", answer.Status)
	require.Equal(t, code, answer.FailureCode, "%s %s", kind, externalID)
}

// requireWaiting: the operation was accepted and waits for its Reference (202).
func (a walletAPI) requireWaiting(t *testing.T, w apiWallet, externalID, kind, amount, reference string) operationAnswer {
	t.Helper()
	status, answer := a.send(t, a.strict, w, externalID, kind, amount, reference)
	require.Equal(t, http.StatusAccepted, status, "%s %s: %+v", kind, externalID, answer)
	require.Equal(t, "PENDING_REFERENCE", answer.Status)
	require.NotEmpty(t, answer.TransactionID)
	return answer
}

func (a walletAPI) reversalsOf(t *testing.T, transactionID string) int {
	t.Helper()
	var n int
	require.NoError(t, a.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM wager_transactions WHERE reference_transaction_id = $1 AND kind IN ('REFUND','ROLLBACK') AND status = 'PROCESSED'`, transactionID).Scan(&n))
	return n
}

func TestARefundGivesTheBetBackAndKeepsTheReferenceItResolved(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	bet := a.mustProcess(t, w, "bet-1", "BET", "30.00", "")

	refund := a.mustProcess(t, w, "refund-1", "REFUND", "30.00", "bet-1")

	require.Equal(t, "100.00", refund.Balance.Amount)
	balance, version := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	require.Equal(t, int64(3), version, "opening, bet, refund")
	entries := a.walk(t, a.strict, w.id, 50)
	require.Len(t, entries, 3)
	require.Equal(t, "CREDIT", entries[2].Direction)
	require.Equal(t, refund.TransactionID, entries[2].TransactionID)
	require.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, a.eventTypes(t, w, refund.TransactionID))
	require.Equal(t, 1, a.reversalsOf(t, bet.TransactionID))

	status, _, _, tx := a.read(t, a.strict, a.provider, "/wagering/transactions/"+refund.TransactionID)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "REFUND", tx.Kind)
	require.Equal(t, "bet-1", tx.ReferenceExternalTransactionID)

	// The reversal replays like any operation.
	status, again := a.send(t, a.strict, w, "refund-1", "REFUND", "30.00", "bet-1")
	require.Equal(t, http.StatusOK, status)
	require.True(t, again.IdempotentReplay)
	require.Equal(t, refund.TransactionID, again.TransactionID)

	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestARollbackDoesTheContraryOfWhatItUndoes(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")

	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")
	a.mustProcess(t, w, "win-1", "WIN", "50.00", "bet-1")
	require.Equal(t, "120.00", a.mustProcess(t, w, "loss-1", "LOSS", "0.00", "").Balance.Amount)

	require.Equal(t, "70.00", a.mustProcess(t, w, "rollback-win", "ROLLBACK", "50.00", "win-1").Balance.Amount, "the prize is taken back")
	require.Equal(t, "100.00", a.mustProcess(t, w, "rollback-bet", "ROLLBACK", "30.00", "bet-1").Balance.Amount, "the stake is given back")

	a.mustProcess(t, w, "bet-2", "BET", "10.00", "")
	a.mustProcess(t, w, "refund-2", "REFUND", "10.00", "bet-2")
	require.Equal(t, "90.00", a.mustProcess(t, w, "rollback-refund", "ROLLBACK", "10.00", "refund-2").Balance.Amount, "the refund is taken back")

	balance, _ := a.wallet(t, w)
	require.Equal(t, int64(9000), balance)
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

// Each rule of the table of References, through the API, with its own code. (The player
// and the currency of a Reference are tried at the domain: through the API a wallet has
// one player and one currency, so those mismatches surface as the wallet's own codes.)
func TestEachRuleOfTheTableOfReferencesAnswersWithItsOwnCode(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	other := a.openFunded(t, "100.00")
	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")
	a.mustProcess(t, w, "win-1", "WIN", "10.00", "")
	a.mustProcess(t, w, "loss-1", "LOSS", "0.00", "")
	a.requireRefusal(t, w, "bet-big", "BET", "500.00", "", "INSUFFICIENT_FUNDS")
	status, _, _ := a.submit(t, a.strict, a.provider, "provider-a:bet-r2", withRound(operation(w, "bet-r2", "BET", "5.00"), "round-2"))
	require.Equal(t, http.StatusOK, status)
	a.mustProcess(t, other, "bet-other", "BET", "5.00", "")

	a.requireWaiting(t, w, "r-notfound", "REFUND", "30.00", "never-seen") // not refused: it waits for its Reference
	a.requireRefusal(t, w, "r-notprocessed", "REFUND", "500.00", "bet-big", "REFERENCE_NOT_PROCESSED")
	a.requireRefusal(t, w, "r-kind-win", "REFUND", "10.00", "win-1", "REFERENCE_KIND_INVALID")
	a.requireRefusal(t, w, "r-kind-loss", "ROLLBACK", "0.01", "loss-1", "REFERENCE_KIND_INVALID")
	a.requireRefusal(t, w, "r-round", "REFUND", "5.00", "bet-r2", "REFERENCE_MISMATCH")
	a.requireRefusal(t, w, "r-wallet", "REFUND", "5.00", "bet-other", "REFERENCE_MISMATCH")
	a.requireRefusal(t, w, "r-amount", "REFUND", "29.99", "bet-1", "REFERENCE_AMOUNT_MISMATCH")

	balance, version := a.wallet(t, w)
	require.Equal(t, int64(7500), balance, "70 after the bet, 80 after the win, 75 after the other bet; the refusals moved nothing")
	require.Equal(t, int64(4), version)

	// Reversed once, a second reversal of either kind is refused.
	a.mustProcess(t, w, "refund-1", "REFUND", "30.00", "bet-1")
	a.requireRefusal(t, w, "refund-again", "REFUND", "30.00", "bet-1", "REFERENCE_ALREADY_REVERSED")
	a.requireRefusal(t, w, "rollback-after-refund", "ROLLBACK", "30.00", "bet-1", "REFERENCE_ALREADY_REVERSED")

	// A reversal that would overdraw has a code of its own.
	poor := a.openFunded(t, "5.00")
	a.mustProcess(t, poor, "p-win", "WIN", "20.00", "")
	a.mustProcess(t, poor, "p-bet", "BET", "24.00", "")
	a.requireRefusal(t, poor, "p-rollback", "ROLLBACK", "20.00", "p-win", "REVERSAL_INSUFFICIENT_FUNDS")
	a.requireRefusal(t, poor, "p-bet-again", "BET", "24.00", "", "INSUFFICIENT_FUNDS")

	for _, wallet := range []apiWallet{w, other, poor} {
		a.requireConsistent(t, wallet)
	}
	a.strict.requireNoViolations()
}

// A Reference is looked for among the provider's own transactions: another provider's id
// is as good as absent.
func TestAReferenceIsLookedForAmongTheProvidersOwnTransactionsOnly(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")

	body := strings.Replace(operationRef(w, "refund-b", "REFUND", "30.00", "bet-1"), `"provider-a"`, `"provider-b"`, 1)
	status, _, answer := a.submit(t, a.strict, a.providerB, "provider-b:refund-b", body)

	require.Equal(t, http.StatusAccepted, status, "to provider-b the bet does not exist (yet): it waits")
	require.Equal(t, "PENDING_REFERENCE", answer.Status)
	balance, _ := a.wallet(t, w)
	require.Equal(t, int64(7000), balance, "and nothing of provider-a's is touched")
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestAWinNamingItsBetIsAcceptedAndIsNeverAReversal(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	bet := a.mustProcess(t, w, "bet-1", "BET", "30.00", "")

	win := a.mustProcess(t, w, "win-1", "WIN", "90.00", "bet-1")
	require.Equal(t, "160.00", win.Balance.Amount, "a prize is not the stake")
	require.Zero(t, a.reversalsOf(t, bet.TransactionID), "a WIN is not a Reversal")
	a.mustProcess(t, w, "refund-1", "REFUND", "30.00", "bet-1")

	status, _, _ := a.submit(t, a.strict, a.provider, "provider-a:bet-r2", withRound(operation(w, "bet-r2", "BET", "5.00"), "round-2"))
	require.Equal(t, http.StatusOK, status)
	a.requireRefusal(t, w, "win-r2", "WIN", "1.00", "bet-r2", "REFERENCE_MISMATCH")
}

// A REFUND and a ROLLBACK of the same BET, racing, have exactly one effect.
func TestARefundAndARollbackRacingOnOneBetHaveOneEffect(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	bet := a.mustProcess(t, w, "bet-1", "BET", "30.00", "")

	const each = 12
	var wg sync.WaitGroup
	var processed, refused atomic.Int64
	start := make(chan struct{})
	for i := range each {
		for _, kind := range []string{"REFUND", "ROLLBACK"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				id := fmt.Sprintf("%s-%d", strings.ToLower(kind), i)
				status, answer := a.send(t, a.client, w, id, kind, "30.00", "bet-1")
				switch {
				case status == http.StatusOK:
					processed.Add(1)
				case status == http.StatusUnprocessableEntity && answer.FailureCode == "REFERENCE_ALREADY_REVERSED":
					refused.Add(1)
				default:
					t.Errorf("%s answered %d %+v", id, status, answer)
				}
			}()
		}
	}
	close(start)
	wg.Wait()

	require.EqualValues(t, 1, processed.Load(), "exactly one reversal took effect")
	require.EqualValues(t, 2*each-1, refused.Load())
	require.Equal(t, 1, a.reversalsOf(t, bet.TransactionID))
	balance, version := a.wallet(t, w)
	require.Equal(t, int64(10000), balance, "the stake came back once")
	require.Equal(t, int64(3), version)
	a.requireConsistent(t, w)
}

// --- through the queue ----------------------------------------------------------------

func messageRef(kind, id string, w apiWallet, amount, reference string) string {
	return strings.Replace(messageFor(kind, id, w, amount), `"roundId"`, fmt.Sprintf(`"referenceExternalTransactionId":%q,"roundId"`, reference), 1)
}

func TestReversalsArriveByMessageWithTheSameResultAndOneEffect(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")
	a.mustProcess(t, w, "tx-bet", "BET", "30.00", "")

	// The messages are in the order of the wallet's group: the refund, copies of it, a
	// rollback of the same bet that must lose, and the same refund once more by HTTP.
	a.send(t, w.id, messageRef("REFUND", "refund", w, "30.00", "tx-bet"))
	a.send(t, w.id, messageRef("REFUND", "refund", w, "30.00", "tx-bet"))
	a.send(t, w.id, messageRef("ROLLBACK", "rollback", w, "30.00", "tx-bet"))
	a.send(t, w.id, messageRef("REFUND", "refund", w, "30.00", "tx-bet"))
	a.drained(t)

	require.Equal(t, int64(10000), a.balanceOf(t, w))
	status, again := a.walletAPI.send(t, a.strict, w, "tx-refund", "REFUND", "30.00", "tx-bet")
	require.Equal(t, http.StatusOK, status)
	require.True(t, again.IdempotentReplay, "the same operation by HTTP is the one the message did")
	var rollback string
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT failure_code FROM wager_transactions WHERE external_transaction_id = 'tx-rollback'`).Scan(&rollback))
	require.Equal(t, "REFERENCE_ALREADY_REVERSED", rollback, "a refusal by a rule is an answer, not a dead letter")
	require.Equal(t, 0, a.depth(t, a.queues.DLQURL))
	a.requireConsistent(t, w)
}
