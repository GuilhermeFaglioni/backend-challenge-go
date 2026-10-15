//go:build multiinstance

package multiinstance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// The queue consumer, in real processes that share a database and a queue and nothing
// else. Every copy of a message is sent with a deduplication id of its own, so the
// queue's deduplication cannot do the application's work.

const consumerVisibility = 5 * time.Second

type consumerHarness struct {
	eventsHarness
	queues infra.InputQueues
}

func newConsumerHarness(t *testing.T) consumerHarness {
	t.Helper()
	h := newEventsHarness(t)
	return consumerHarness{eventsHarness: h, queues: infra.CreateInputQueues(t.Context(), t, h.sqs, 5, consumerVisibility)}
}

func (h consumerHarness) env(extra map[string]string) map[string]string {
	env := publisherEnv(h.queue, map[string]string{
		"SQS_INPUT_QUEUE":                 h.queues.InputName,
		"SQS_DEAD_LETTER_QUEUE":           h.queues.DLQName,
		"SQS_CONSUMER_WORKERS":            "3",
		"SQS_CONSUMER_POLL_WAIT":          "1s",
		"SQS_CONSUMER_PROCESSING_TIMEOUT": "3s",
		"SQS_CONSUMER_RETRY_BACKOFF_BASE": "1s",
		"SQS_CONSUMER_RETRY_BACKOFF_MAX":  "2s",
	})
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func (h consumerHarness) start(t *testing.T, name string, extra map[string]string) *infra.Instance {
	t.Helper()
	instance := startInstance(t, name, freeAddrs(t, 1)[0], h.db, h.env(extra))
	waitReady(t, instance)
	return instance
}

func (h consumerHarness) send(t *testing.T, group, body string) {
	t.Helper()
	require.NoError(t, infra.Send(t.Context(), h.sqs, h.queues.InputURL, group, uuid.NewString(), body, nil))
}

func (h consumerHarness) depth(t *testing.T, url string) int {
	t.Helper()
	n, err := infra.QueueDepth(t.Context(), h.sqs, url)
	require.NoError(t, err)
	return n
}

func (h consumerHarness) drained(t *testing.T, within time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool { return h.depth(t, h.queues.InputURL) == 0 }, within, 250*time.Millisecond, "the input queue to be emptied")
}

func (h consumerHarness) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, h.owner.QueryRow(t.Context(), sql, args...).Scan(&n))
	return n
}

func betMessage(id string, w wallet, externalID, amount string) string {
	return fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{
		"providerId":"provider-a","externalTransactionId":%q,"idempotencyKey":%q,"playerId":%q,"walletId":%q,
		"roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":%q,"currency":"BRL"}}}`,
		id, externalID, "provider-a:"+externalID, w.player, w.id, amount)
}

func (h consumerHarness) openWallet(t *testing.T, via *infra.Instance, amount string) wallet {
	t.Helper()
	g := &guarantees{h: h.eventsHarness, client: &http.Client{Timeout: 30 * time.Second}}
	return g.open(t, via, amount)
}

// metricOf reads one series from an instance's /metrics.
func metricOf(t *testing.T, instance *infra.Instance, series string) float64 {
	t.Helper()
	resp, err := http.Get("http://" + instance.Addr + "/metrics") //nolint:noctx // a test fixture request
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	text, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	for _, line := range strings.Split(string(text), "\n") {
		if strings.HasPrefix(line, series+" ") {
			var v float64
			_, err := fmt.Sscanf(strings.TrimPrefix(line, series+" "), "%g", &v)
			require.NoError(t, err)
			return v
		}
	}
	t.Fatalf("%s has no series %s", instance.Name, series)
	return 0
}

// crashesWhenAMessageArrives is the fault-point check for a point on the path of a
// queue message.
func crashesWhenAMessageArrives(t *testing.T, point faultinject.Point) {
	t.Helper()
	h := newConsumerHarness(t)
	crashing := h.start(t, "crashing", map[string]string{faultinject.EnvVar: string(point)})
	w := h.openWallet(t, crashing, "100.00")

	h.send(t, w.id, betMessage("msg-1", w, "tx-1", "25.00"))

	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, crashing.WaitMessage(ctx, "fault point reached: "+string(point)))
	code, err := crashing.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code, "exit status of a fault point, not of a normal stop or a config error")
}

// The process dies after the commit and before the delete. The work is done and
// durable; the queue still has the message. Another instance receives it and, finding it
// handled, changes nothing and deletes it.
func TestACrashAfterTheCommitAndBeforeTheDeleteChangesNothingWhenTheMessageComesBack(t *testing.T) {
	h := newConsumerHarness(t)
	doomed := h.start(t, "doomed", map[string]string{faultinject.EnvVar: string(faultinject.ConsumerAfterCommit)})
	w := h.openWallet(t, doomed, "100.00")

	h.send(t, w.id, betMessage("msg-1", w, "tx-1", "25.00"))

	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, doomed.WaitMessage(ctx, "fault point reached: "+string(faultinject.ConsumerAfterCommit)))
	code, err := doomed.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code)

	// What the dead process committed is there, and the message still is too.
	var balance int64
	require.NoError(t, h.owner.QueryRow(t.Context(), `SELECT balance_minor FROM wallets WHERE id = $1`, w.id).Scan(&balance))
	require.Equal(t, int64(7500), balance, "the commit happened before the crash")
	require.Equal(t, 1, h.count(t, `SELECT count(*) FROM inbox WHERE message_id = 'msg-1' AND completed_at IS NOT NULL`))
	require.Equal(t, 1, h.depth(t, h.queues.InputURL), "and the queue still holds the message")
	eventsBefore := h.count(t, `SELECT count(*) FROM outbox`)
	require.Equal(t, 4, eventsBefore, "two events of the opening and two of the bet")

	rescuer := h.start(t, "rescuer", nil)
	h.drained(t, 3*consumerVisibility+eventsWithin)

	require.NoError(t, h.owner.QueryRow(t.Context(), `SELECT balance_minor FROM wallets WHERE id = $1`, w.id).Scan(&balance))
	require.Equal(t, int64(7500), balance, "the same result, no second movement")
	require.Equal(t, 1, h.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id))
	require.Equal(t, 1, h.count(t, `SELECT count(*) FROM inbox`))
	require.Equal(t, eventsBefore, h.count(t, `SELECT count(*) FROM outbox`), "and no second set of events")
	require.EqualValues(t, 1, metricOf(t, rescuer, `sqs_messages_handled_total{result="duplicate"}`))
	require.EqualValues(t, 0, metricOf(t, rescuer, `sqs_messages_handled_total{result="handled"}`))
	require.Equal(t, 0, h.depth(t, h.queues.DLQURL))
}

// A consumer killed in the middle of a stream of messages is replaced by the others:
// what it had in flight comes back after the visibility timeout and is handled once.
func TestAConsumerKilledMidFlightIsReplacedByAnother(t *testing.T) {
	h := newConsumerHarness(t)
	a, b := h.start(t, "alpha", nil), h.start(t, "bravo", nil)
	wallets, per := h.sendStream(t, a, 6, 60)

	require.Eventually(t, func() bool {
		return h.count(t, `SELECT count(*) FROM inbox`) >= 10
	}, eventsWithin, 10*time.Millisecond, "some messages to have been handled")
	a.Kill()
	require.Less(t, h.count(t, `SELECT count(*) FROM inbox`), len(wallets)*per, "the stream was not over when the process died")
	require.Positive(t, h.depth(t, h.queues.InputURL))

	h.drained(t, 4*consumerVisibility+eventsWithin)
	h.requireStream(t, wallets, per)
	require.Zero(t, metricOf(t, b, `sqs_messages_handled_total{result="dead_lettered"}`))
}

// Three instances take messages from one queue and every message is handled once, with
// copies sprinkled in; at the end the books of every wallet agree.
func TestThreeInstancesCompetingNeverHandleAMessageTwice(t *testing.T) {
	h := newConsumerHarness(t)
	instances := []*infra.Instance{h.start(t, "alpha", nil), h.start(t, "bravo", nil), h.start(t, "charlie", nil)}
	wallets, per := h.sendStream(t, instances[0], 8, 15)

	h.drained(t, eventsWithin+30*time.Second)
	h.requireStream(t, wallets, per)

	var handled, duplicate float64
	for _, i := range instances {
		handled += metricOf(t, i, `sqs_messages_handled_total{result="handled"}`)
		duplicate += metricOf(t, i, `sqs_messages_handled_total{result="duplicate"}`)
	}
	require.EqualValues(t, len(wallets)*per, handled, "every message was applied by exactly one instance")
	require.Greater(t, duplicate, 0.0, "and the copies were recognised")
	t.Logf("handled %v, duplicates %v", handled, duplicate)
}

// SIGTERM on a consumer in the middle of the stream: it stops receiving, finishes what it
// is handling and exits cleanly; the others handle the rest and nothing is lost.
func TestATerminatedConsumerFinishesWhatItHasAndTheOthersTakeTheRest(t *testing.T) {
	h := newConsumerHarness(t)
	a, b, c := h.start(t, "alpha", nil), h.start(t, "bravo", nil), h.start(t, "charlie", nil)
	wallets, per := h.sendStream(t, a, 8, 60)
	require.Eventually(t, func() bool { return h.count(t, `SELECT count(*) FROM inbox`) >= 15 }, eventsWithin, 10*time.Millisecond)
	require.Positive(t, h.depth(t, h.queues.InputURL), "there was still work when the signal came")

	ctx, cancel := context.WithTimeout(t.Context(), stopTimeout)
	defer cancel()
	code, err := b.Terminate(ctx)
	require.NoError(t, err)
	require.Zero(t, code, "a clean exit")
	require.Contains(t, b.Logs.Messages(), "sqs consumer stopped")

	h.drained(t, 4*consumerVisibility+eventsWithin)
	h.requireStream(t, wallets, per)
	require.Equal(t, 0, h.depth(t, h.queues.DLQURL))
	_, _ = a, c // the others, not asked for anything
}

// sendStream opens wallets and sends perWallet bets of 1.00 to each, in order within its
// group, a copy of every third one among them.
func (h consumerHarness) sendStream(t *testing.T, via *infra.Instance, wallets, perWallet int) ([]wallet, int) {
	t.Helper()
	var ws []wallet
	for range wallets {
		ws = append(ws, h.openWallet(t, via, "1000.00"))
	}
	var wg sync.WaitGroup
	for j, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWallet {
				body := betMessage(fmt.Sprintf("msg-%d-%d", j, i), w, fmt.Sprintf("tx-%d-%d", j, i), "1.00")
				h.send(t, w.id, body)
				if i%3 == 0 {
					h.send(t, w.id, body)
				}
			}
		}()
	}
	wg.Wait()
	return ws, perWallet
}

// requireStream checks the end state of sendStream: every wallet debited once per
// message, the books agree, one inbox row per message.
func (h consumerHarness) requireStream(t *testing.T, wallets []wallet, perWallet int) {
	t.Helper()
	for _, w := range wallets {
		var balance int64
		require.NoError(t, h.owner.QueryRow(t.Context(), `SELECT balance_minor FROM wallets WHERE id = $1`, w.id).Scan(&balance))
		require.Equal(t, int64(100000-perWallet*100), balance, "wallet %s", w.id)
		require.Equal(t, perWallet, h.count(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id))
	}
	require.Equal(t, len(wallets)*perWallet, h.count(t, `SELECT count(*) FROM inbox`))
	require.Equal(t, len(wallets)*perWallet, h.count(t, `SELECT count(*) FROM wager_transactions WHERE kind = 'BET'`))
}
