//go:build multiinstance

package multiinstance

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// Operations that wait for a Reference, in real processes that share a database and
// nothing else. The state of the wait is in the database, so it has to outlive the process
// that accepted it, the worker that reserved it, and every process there is.

// pendingEnv is a fast policy: gaps of a second doubling to two, a lease of three seconds,
// and a wait of up to a minute or eight attempts.
func pendingEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"PENDING_REFERENCE_MAX_ATTEMPTS":  "8",
		"PENDING_REFERENCE_TTL":           "1m",
		"PENDING_REFERENCE_BACKOFF_BASE":  "1s",
		"PENDING_REFERENCE_BACKOFF_MAX":   "2s",
		"PENDING_REFERENCE_POLL_INTERVAL": "100ms",
		"PENDING_REFERENCE_LEASE":         "3s",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func newPending(t *testing.T) *guarantees {
	t.Helper()
	h := newEventsHarness(t)
	g := &guarantees{h: h, client: &http.Client{Timeout: 60 * time.Second}}
	var err error
	g.provider, err = kc.Token(t.Context(), "provider-a", "provider-a-local-secret")
	require.NoError(t, err)
	return g
}

func (g *guarantees) startWith(t *testing.T, name string, env map[string]string) *infra.Instance {
	t.Helper()
	instance := g.h.start(t, name, pendingEnv(env))
	g.instances = append(g.instances, instance)
	return instance
}

func (g *guarantees) status(t *testing.T, externalID string) (status, failure string) {
	t.Helper()
	var code *string
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT status, failure_code FROM wager_transactions WHERE external_transaction_id = $1`, externalID).Scan(&status, &code))
	if code != nil {
		failure = *code
	}
	return status, failure
}

func (g *guarantees) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, g.h.owner.QueryRow(t.Context(), sql, args...).Scan(&n))
	return n
}

// crashesWhenAWaitArrives is the fault-point check for a point on the path of a waiting
// operation: the process with the point is given a reversal that waits, and a bet that
// makes it due.
func crashesWhenAWaitArrives(t *testing.T, point faultinject.Point) {
	t.Helper()
	g := newPending(t)
	bystander := g.startWith(t, "bystander", map[string]string{"PENDING_REFERENCE_POLL_INTERVAL": "1h", "PENDING_REFERENCE_BACKOFF_BASE": "1h", "PENDING_REFERENCE_BACKOFF_MAX": "1h"})
	crashing := g.startWith(t, "crashing", map[string]string{faultinject.EnvVar: string(point)})
	w := g.open(t, bystander, "100.00")

	_ = g.tryReverse(t, crashing, w, "refund-1", "REFUND", "30.00", "bet-1")
	_ = g.operation(t, bystander, w, "bet-1", "BET", "30.00")

	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, crashing.WaitMessage(ctx, "fault point reached: "+string(point)))
	code, err := crashing.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code, "exit status of a fault point, not of a normal stop or a config error")
	require.True(t, bystander.Ready(t.Context()), "an unrelated instance must not notice")
}

func (g *guarantees) waitFor(t *testing.T, externalID, want string) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, _ := g.status(t, externalID)
		return status == want
	}, 60*time.Second, 100*time.Millisecond, "%s to become %s", externalID, want)
}

// A reversal sent before its bet, to one process, is applied once the bet arrives at
// another, by the worker of whichever process reserves it.
func TestAReversalBeforeItsBetIsAppliedOnceAcrossProcesses(t *testing.T) {
	g := newPending(t)
	a, b := g.startWith(t, "alpha", nil), g.startWith(t, "bravo", nil)
	w := g.open(t, a, "100.00")

	waiting := g.reverse(t, a, w, "refund-1", "REFUND", "30.00", "bet-1")
	require.Equal(t, http.StatusAccepted, waiting.status)
	require.Equal(t, "PENDING_REFERENCE", waiting.answer.Status)
	require.Equal(t, http.StatusOK, g.operation(t, b, w, "bet-1", "BET", "30.00").status)

	g.waitFor(t, "refund-1", "PROCESSED")
	balance, _ := g.books(t, w)
	require.Equal(t, int64(10000), balance)
	again := g.reverse(t, b, w, "refund-1", "REFUND", "30.00", "bet-1")
	require.Equal(t, http.StatusOK, again.status)
	require.True(t, again.answer.IdempotentReplay)
}

// The process that accepted the wait dies right after committing it, before it can answer.
// Another process, which never saw the request, resolves it.
func TestAWaitCommittedByAProcessThatThenDiesIsResolvedByAnother(t *testing.T) {
	g := newPending(t)
	survivor := g.startWith(t, "survivor", nil)
	doomed := g.startWith(t, "doomed", map[string]string{faultinject.EnvVar: string(faultinject.PendingAfterCommit)})
	w := g.open(t, survivor, "100.00")

	// The doomed process accepts the operation, commits the wait, and exits before answering.
	_ = g.tryReverse(t, doomed, w, "refund-1", "REFUND", "30.00", "bet-1")
	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, doomed.WaitMessage(ctx, "fault point reached: "+string(faultinject.PendingAfterCommit)))
	code, err := doomed.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code)
	status, _ := g.status(t, "refund-1")
	require.Equal(t, "PENDING_REFERENCE", status, "the wait outlived the process that stored it")

	require.Equal(t, http.StatusOK, g.operation(t, survivor, w, "bet-1", "BET", "30.00").status)
	g.waitFor(t, "refund-1", "PROCESSED")

	// The client, who never got an answer, sends it again and gets the outcome.
	again := g.reverse(t, survivor, w, "refund-1", "REFUND", "30.00", "bet-1")
	require.Equal(t, http.StatusOK, again.status)
	require.True(t, again.answer.IdempotentReplay)
	balance, _ := g.books(t, w)
	require.Equal(t, int64(10000), balance, "applied once")
}

// The worker that reserved the operation dies holding it. Its lease runs out and a worker of
// another process takes it, and applies it once.
func TestAWorkerKilledHoldingAnOperationIsReplacedByAnotherProcess(t *testing.T) {
	g := newPending(t)
	doomed := g.startWith(t, "doomed", map[string]string{faultinject.EnvVar: string(faultinject.PendingAfterReserve)})
	w := g.open(t, doomed, "100.00")
	require.Equal(t, http.StatusAccepted, g.reverse(t, doomed, w, "refund-1", "REFUND", "30.00", "bet-1").status)
	require.Equal(t, http.StatusOK, g.operation(t, doomed, w, "bet-1", "BET", "30.00").status)

	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, doomed.WaitMessage(ctx, "fault point reached: "+string(faultinject.PendingAfterReserve)), "its worker reserves the operation when it is due")
	code, err := doomed.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code)
	status, _ := g.status(t, "refund-1")
	require.Equal(t, "PENDING_REFERENCE", status, "nothing was tried")

	g.startWith(t, "replacement", nil)
	g.waitFor(t, "refund-1", "PROCESSED")

	balance, _ := g.books(t, w)
	require.Equal(t, int64(10000), balance)
	require.Equal(t, 1, g.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'`, w.id)-1, "one credit besides the opening")
}

// Every process stops, and new ones start: the wait is exactly where it was.
func TestAWaitSurvivesTheRestartOfEveryProcess(t *testing.T) {
	g := newPending(t)
	first := g.startWith(t, "first", nil)
	w := g.open(t, first, "100.00")
	require.Equal(t, http.StatusAccepted, g.reverse(t, first, w, "refund-1", "REFUND", "30.00", "bet-1").status)
	var attempts int
	require.NoError(t, g.h.owner.QueryRow(t.Context(), `SELECT attempts FROM wager_transactions WHERE external_transaction_id = 'refund-1'`).Scan(&attempts))

	ctx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	code, err := first.Terminate(ctx)
	require.NoError(t, err)
	require.Zero(t, code)

	second := g.startWith(t, "second", nil)
	require.Equal(t, http.StatusOK, g.operation(t, second, w, "bet-1", "BET", "30.00").status)
	g.waitFor(t, "refund-1", "PROCESSED")
	balance, _ := g.books(t, w)
	require.Equal(t, int64(10000), balance)
	require.GreaterOrEqual(t, attempts, 1)
}

// A wait that runs out, across processes: the reversal is refused, with its event.
func TestAReferenceThatNeverComesIsRefusedWhenTheWaitRunsOut(t *testing.T) {
	g := newPending(t)
	a := g.startWith(t, "alpha", map[string]string{"PENDING_REFERENCE_TTL": "8s", "PENDING_REFERENCE_MAX_ATTEMPTS": "20"})
	g.startWith(t, "bravo", map[string]string{"PENDING_REFERENCE_TTL": "8s", "PENDING_REFERENCE_MAX_ATTEMPTS": "20"})
	w := g.open(t, a, "100.00")

	require.Equal(t, http.StatusAccepted, g.reverse(t, a, w, "refund-1", "REFUND", "30.00", "bet-never").status)

	g.waitFor(t, "refund-1", "REJECTED")
	_, failure := g.status(t, "refund-1")
	require.Equal(t, "REFERENCE_NOT_FOUND", failure)
	require.Equal(t, 1, g.count(t, `SELECT count(*) FROM outbox WHERE event_type = 'WagerTransactionRejected' AND payload->>'externalTransactionId' = 'refund-1'`))
	balance, _ := g.books(t, w)
	require.Equal(t, int64(10000), balance)
}

// Many operations waiting for many bets, three processes with a worker each: every one is
// applied exactly once, whichever worker took it, and the books agree.
func TestThreeWorkersNeverApplyAWaitingOperationTwice(t *testing.T) {
	g := newPending(t)
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		g.startWith(t, name, map[string]string{"PENDING_REFERENCE_BATCH_SIZE": "3"})
	}
	const wallets, perWallet = 4, 8
	var ws []wallet
	for range wallets {
		ws = append(ws, g.open(t, g.on(0), "1000.00"))
	}

	var wg sync.WaitGroup
	for j, w := range ws {
		for i := range perWallet {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := g.reverse(t, g.on(i), w, fmt.Sprintf("refund-%d-%d", j, i), "REFUND", "1.00", fmt.Sprintf("bet-%d-%d", j, i))
				if r.status != http.StatusAccepted {
					t.Errorf("refund-%d-%d answered %d", j, i, r.status)
				}
			}()
		}
	}
	wg.Wait()
	for j, w := range ws {
		for i := range perWallet {
			require.Equal(t, http.StatusOK, g.operation(t, g.on(i+j), w, fmt.Sprintf("bet-%d-%d", j, i), "BET", "1.00").status)
		}
	}

	for j := range ws {
		for i := range perWallet {
			g.waitFor(t, fmt.Sprintf("refund-%d-%d", j, i), "PROCESSED")
		}
	}
	for _, w := range ws {
		balance, debits := g.books(t, w)
		require.Equal(t, int64(100000), balance, "every stake came back, once")
		require.Equal(t, int64(perWallet*100), debits)
		require.Equal(t, perWallet, g.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'`, w.id)-1)
	}

	var processed float64
	for _, i := range g.instances {
		processed += metricOf(t, i, `pending_reference_resolutions_total{outcome="processed"}`)
	}
	require.EqualValues(t, wallets*perWallet, processed, "each was processed by exactly one worker")
}
