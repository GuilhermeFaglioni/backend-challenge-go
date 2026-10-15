//go:build multiinstance

package multiinstance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// The scenarios the challenge requires of a BET, run through the HTTP API against three
// independent operating-system processes that share nothing but PostgreSQL. Every
// scenario ends by checking the books: the balance of each wallet it used is the sum of
// the credits of its ledger minus the sum of the debits.

type guarantees struct {
	h         eventsHarness
	instances []*infra.Instance
	provider  string // token of provider-a
	client    *http.Client
}

func startGuarantees(t *testing.T) *guarantees {
	t.Helper()
	h := newEventsHarness(t)
	g := &guarantees{h: h, client: &http.Client{Timeout: 60 * time.Second}}
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		g.instances = append(g.instances, h.start(t, name, nil))
	}
	var err error
	g.provider, err = kc.Token(t.Context(), "provider-a", "provider-a-local-secret")
	require.NoError(t, err)
	return g
}

func (g *guarantees) on(i int) *infra.Instance { return g.instances[i%len(g.instances)] }

type wallet struct{ id, player string }

type answer struct {
	Status           string `json:"status"`
	TransactionID    string `json:"transactionId"`
	FailureCode      string `json:"failureCode"`
	IdempotentReplay bool   `json:"idempotentReplay"`
	Balance          struct {
		Amount string `json:"amount"`
	} `json:"balance"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

type response struct {
	status int
	answer answer
}

func (g *guarantees) post(t *testing.T, instance *infra.Instance, token, path, key, body string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+instance.Addr+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := g.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var parsed answer
	_ = json.Unmarshal(data, &parsed)
	return response{status: resp.StatusCode, answer: parsed}
}

func (g *guarantees) open(t *testing.T, instance *infra.Instance, amount string) wallet {
	t.Helper()
	player := uuid.New().String()
	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, player, amount)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+instance.Addr+"/wallets", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+g.h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(data))
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(data, &created))
	return wallet{id: created.ID, player: player}
}

func (g *guarantees) bet(t *testing.T, instance *infra.Instance, w wallet, externalID, key, amount string) response {
	t.Helper()
	body := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,"walletId":%q,"roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":%q,"currency":"BRL"}}`,
		externalID, w.player, w.id, amount)
	return g.post(t, instance, g.provider, "/wagering/transactions", key, body)
}

// reconcile asks a process, through the API, to check the wallet's books.
func (g *guarantees) reconcile(t *testing.T, instance *infra.Instance, w wallet) (stored, calculated string, consistent bool) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+instance.Addr+"/wallets/"+w.id+"/reconciliation", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+g.h.token)
	resp, err := g.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(data))
	var result struct {
		StoredBalance     struct{ Amount string } `json:"storedBalance"`
		CalculatedBalance struct{ Amount string } `json:"calculatedBalance"`
		Consistent        bool                    `json:"consistent"`
	}
	require.NoError(t, json.Unmarshal(data, &result))
	return result.StoredBalance.Amount, result.CalculatedBalance.Amount, result.Consistent
}

// books checks one wallet: the stored balance is the credits minus the debits of its
// ledger, it is not negative, and each ledger entry starts where the previous one ended.
// It then has a process, other than the one that wrote last, say the same through the
// reconciliation endpoint.
func (g *guarantees) books(t *testing.T, w wallet) (balance, debits int64) {
	t.Helper()
	var credits int64
	var version, entries int64
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT balance_minor, version FROM wallets WHERE id = $1`, w.id).Scan(&balance, &version))
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `
		SELECT coalesce(sum(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0),
		       coalesce(sum(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0), count(*)
		FROM wallet_ledger_entries WHERE wallet_id = $1`, w.id).Scan(&credits, &debits, &entries))
	require.Equal(t, credits-debits, balance, "wallet %s: the balance is not what its ledger says", w.id)
	require.GreaterOrEqual(t, balance, int64(0))
	require.Equal(t, entries, version, "wallet %s: one version step per ledger entry (the opening is version 1)", w.id)

	var broken int
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `
		SELECT count(*) FROM (
			SELECT balance_before_minor, lag(balance_after_minor) OVER (ORDER BY balance_before_minor DESC) AS previous_after
			FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT') c
		WHERE previous_after IS NOT NULL AND previous_after <> balance_before_minor`, w.id).Scan(&broken))
	require.Zero(t, broken, "wallet %s: the debits do not chain", w.id)

	stored, calculated, consistent := g.reconcile(t, g.on(int(version)), w)
	require.True(t, consistent, "wallet %s: the reconciliation endpoint finds the books apart (stored %s, calculated %s)", w.id, stored, calculated)
	require.Equal(t, fmt.Sprintf("%d.%02d", balance/100, balance%100), stored, "wallet %s: the endpoint reads the balance the test read", w.id)
	return balance, debits
}

func (g *guarantees) debitEntries(t *testing.T, w wallet) int {
	t.Helper()
	var n int
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id).Scan(&n))
	return n
}

// --- the same bet fifty times at once -------------------------------------------------

func TestTheSameBetSentFiftyTimesInParallelIsDebitedOnce(t *testing.T) {
	g := startGuarantees(t)
	w := g.open(t, g.on(0), "100.00")
	const copies = 50

	var wg sync.WaitGroup
	responses := make(chan response, copies)
	start := make(chan struct{})
	for i := 0; i < copies; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Spread over the three processes.
			responses <- g.bet(t, g.on(i), w, "tx-1", "provider-a:tx-1", "25.00")
		}()
	}
	close(start)
	wg.Wait()
	close(responses)

	originals, replays := 0, 0
	ids := map[string]bool{}
	for r := range responses {
		require.Equal(t, http.StatusOK, r.status, "answer: %+v", r.answer)
		require.Equal(t, "75.00", r.answer.Balance.Amount, "every copy is told the same balance")
		ids[r.answer.TransactionID] = true
		if r.answer.IdempotentReplay {
			replays++
		} else {
			originals++
		}
	}
	require.Equal(t, 1, originals, "exactly one request did the work")
	require.Equal(t, copies-1, replays)
	require.Len(t, ids, 1, "and they all describe the same transaction")

	require.Equal(t, 1, g.debitEntries(t, w), "exactly one debit")
	balance, _ := g.books(t, w)
	require.Equal(t, int64(7500), balance)
}

// --- two bets that do not both fit --------------------------------------------------------

func TestTwoBetsOf80On100AtOnceOnlyOneIsProcessed(t *testing.T) {
	g := startGuarantees(t)
	const wallets = 12
	all := make([]wallet, wallets)
	for i := range all {
		all[i] = g.open(t, g.on(i), "100.00")
	}

	type outcome struct{ a, b response }
	outcomes := make([]outcome, wallets)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, w := range all {
		// The two bets of a wallet go to two different processes, at the same moment.
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			outcomes[i].a = g.bet(t, g.on(i), w, fmt.Sprintf("tx-a-%d", i), fmt.Sprintf("key-a-%d", i), "80.00")
		}()
		go func() {
			defer wg.Done()
			<-start
			outcomes[i].b = g.bet(t, g.on(i+1), w, fmt.Sprintf("tx-b-%d", i), fmt.Sprintf("key-b-%d", i), "80.00")
		}()
	}
	close(start)
	wg.Wait()

	for i, w := range all {
		o := outcomes[i]
		statuses := []int{o.a.status, o.b.status}
		require.ElementsMatch(t, []int{http.StatusOK, http.StatusUnprocessableEntity}, statuses, "wallet %s: one processed, one rejected", w.id)
		rejected := o.a
		if o.b.status == http.StatusUnprocessableEntity {
			rejected = o.b
		}
		require.Equal(t, "INSUFFICIENT_FUNDS", rejected.answer.FailureCode)

		balance, debits := g.books(t, w)
		require.Equal(t, int64(2000), balance, "wallet %s ends with 20.00", w.id)
		require.Equal(t, int64(8000), debits)
		require.Equal(t, 1, g.debitEntries(t, w), "a single debit in the ledger")
	}
}

// --- different wallets do not wait for one another -----------------------------------------

// There is no global lock: while one wallet is locked by someone else, operations on other
// wallets, on any of the processes, go through. The lock is held with a transaction of
// our own, so this needs no sleeps: the blocked bet cannot finish until we release it, and
// the others must finish while it is still blocked.
func TestOperationsOnDifferentWalletsDoNotWaitForEachOther(t *testing.T) {
	g := startGuarantees(t)
	blocked := g.open(t, g.on(0), "100.00")
	others := make([]wallet, 9)
	for i := range others {
		others[i] = g.open(t, g.on(i), "100.00")
	}

	holder, err := g.h.owner.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	_, err = holder.Exec(t.Context(), `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, blocked.id)
	require.NoError(t, err)

	stuck := make(chan response, 1)
	go func() { stuck <- g.bet(t, g.on(0), blocked, "tx-blocked", "key-blocked", "10.00") }()

	var wg sync.WaitGroup
	results := make([]response, len(others))
	for i, w := range others {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = g.bet(t, g.on(i+1), w, fmt.Sprintf("tx-free-%d", i), fmt.Sprintf("key-free-%d", i), "10.00")
		}()
	}
	wg.Wait()
	for i, r := range results {
		require.Equal(t, http.StatusOK, r.status, "wallet %d", i)
	}
	select {
	case r := <-stuck:
		t.Fatalf("the bet on the locked wallet finished (%d) while its lock was still held", r.status)
	default: // still waiting for its wallet, as it should
	}

	require.NoError(t, holder.Commit(t.Context()), "release the wallet")
	select {
	case r := <-stuck:
		require.Equal(t, http.StatusOK, r.status)
	case <-time.After(30 * time.Second):
		t.Fatal("the bet did not go on after its wallet was released")
	}

	for _, w := range append(others, blocked) {
		balance, _ := g.books(t, w)
		require.Equal(t, int64(9000), balance)
	}
}

// --- restart ---------------------------------------------------------------------------------

// Nothing that matters lives in a process: after every instance has been stopped and
// started again, what was processed still replays with its original answer, and the
// balance is what it was.
func TestAfterRestartingEveryInstanceEverythingReplaysAndTheBalanceIsUnchanged(t *testing.T) {
	g := startGuarantees(t)
	w := g.open(t, g.on(0), "100.00")

	first := g.bet(t, g.on(0), w, "tx-1", "key-1", "30.00")
	second := g.bet(t, g.on(1), w, "tx-2", "key-2", "20.00")
	rejected := g.bet(t, g.on(2), w, "tx-big", "key-big", "500.00")
	require.Equal(t, http.StatusOK, first.status)
	require.Equal(t, http.StatusOK, second.status)
	require.Equal(t, http.StatusUnprocessableEntity, rejected.status)
	require.Equal(t, "50.00", second.answer.Balance.Amount)

	balanceBefore, debitsBefore := g.books(t, w)
	var entriesBefore, transactionsBefore int
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries`).Scan(&entriesBefore))
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT count(*) FROM wager_transactions`).Scan(&transactionsBefore))

	// Stop all of them (the way an orchestrator would), then start them again.
	stopCtx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	for _, instance := range g.instances {
		code, err := instance.Terminate(stopCtx)
		require.NoError(t, err)
		require.Equal(t, 0, code, "%s did not stop cleanly:\n%s", instance.Name, instance.Logs.String())
	}
	restarted := make([]*infra.Instance, len(g.instances))
	for i, instance := range g.instances {
		reborn, err := instance.Restart(lifetime(t))
		require.NoError(t, err)
		track(t, reborn)
		restarted[i] = reborn
	}
	waitReady(t, restarted...)
	g.instances = restarted

	// Everything replays, through a different process than the one that first answered it.
	for i, c := range []struct {
		name  string
		resp  response
		call  func(*infra.Instance) response
		code  int
		check func(response)
	}{
		{"first bet", first, func(in *infra.Instance) response { return g.bet(t, in, w, "tx-1", "key-1", "30.00") }, http.StatusOK,
			func(r response) { require.Equal(t, "70.00", r.answer.Balance.Amount, "the balance of the first time") }},
		{"second bet", second, func(in *infra.Instance) response { return g.bet(t, in, w, "tx-2", "key-2", "20.00") }, http.StatusOK,
			func(r response) { require.Equal(t, "50.00", r.answer.Balance.Amount) }},
		{"the rejection", rejected, func(in *infra.Instance) response { return g.bet(t, in, w, "tx-big", "key-big", "500.00") }, http.StatusUnprocessableEntity,
			func(r response) { require.Equal(t, "INSUFFICIENT_FUNDS", r.answer.FailureCode) }},
	} {
		got := c.call(g.on(i + 1))
		require.Equal(t, c.code, got.status, c.name)
		require.True(t, got.answer.IdempotentReplay, c.name)
		require.Equal(t, c.resp.answer.TransactionID, got.answer.TransactionID, "%s: the same transaction", c.name)
		c.check(got)
	}

	balanceAfter, debitsAfter := g.books(t, w)
	require.Equal(t, balanceBefore, balanceAfter)
	require.Equal(t, debitsBefore, debitsAfter)
	var entriesAfter, transactionsAfter int
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries`).Scan(&entriesAfter))
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT count(*) FROM wager_transactions`).Scan(&transactionsAfter))
	require.Equal(t, entriesBefore, entriesAfter, "the replays moved no money")
	require.Equal(t, transactionsBefore, transactionsAfter)

	// And the restarted processes work: a new bet is processed normally.
	next := g.bet(t, g.on(0), w, "tx-3", "key-3", "10.00")
	require.Equal(t, http.StatusOK, next.status)
	require.Equal(t, "40.00", next.answer.Balance.Amount)
	g.books(t, w)
}
