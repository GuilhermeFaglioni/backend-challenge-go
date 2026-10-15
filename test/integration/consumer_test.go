//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// The consumer, against a real queue, a real database and the whole application. Every
// test has queues of its own, so what it finds in them is what it caused. Every copy of a
// message is sent with a deduplication id of its own: the queue's own deduplication would
// otherwise throw the copies away and the tests would prove nothing about the application.

const queueVisibility = 4 * time.Second

type consumerAPI struct {
	walletAPI
	sqs    *sqs.Client
	queues infra.InputQueues
	seq    int
}

func startConsumerAPI(t *testing.T, tune func(*config.Consumer)) *consumerAPI {
	t.Helper()
	return startConsumerAPIWith(t, func(c *config.Config) {
		if tune != nil {
			tune(&c.SQS.Consumer)
		}
	})
}

// startConsumerAPIWith starts the application against queues of the test's own, after
// letting the test change the configuration.
func startConsumerAPIWith(t *testing.T, configure func(*config.Config)) *consumerAPI {
	t.Helper()
	client, err := infra.NewSQSClient(t.Context(), ls.Endpoint)
	require.NoError(t, err)
	queues := infra.CreateInputQueues(t.Context(), t, client, 3, queueVisibility)
	a := startWalletAPIWith(t, func(c *config.Config) {
		c.SQS.InputQueue, c.SQS.DeadLetterQueue = queues.InputName, queues.DLQName
		c.SQS.Consumer = config.Consumer{
			Credentials: c.SQS.Consumer.Credentials,
			Workers:     2, BatchSize: 10, PollWait: time.Second, ProcessingTimeout: 2 * time.Second,
			RetryBackoffBase: time.Second, RetryBackoffMax: 2 * time.Second,
		}
		if configure != nil {
			configure(c)
		}
	})
	return &consumerAPI{walletAPI: a, sqs: client, queues: queues}
}

// requestBody is the message body for an operation of an API wallet.
func requestBody(messageID string, w apiWallet, externalID, key, kind, amount string) string {
	return fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{
		"providerId":"provider-a","externalTransactionId":%q,"idempotencyKey":%q,"playerId":%q,"walletId":%q,
		"roundId":"round-1","gameId":"game-1","kind":%q,"money":{"amount":%q,"currency":"BRL"}}}`,
		messageID, externalID, key, w.player, w.id, kind, amount)
}

// send puts a message in the input queue, as a copy of its own for the queue.
func (a *consumerAPI) send(t *testing.T, group, body string) {
	t.Helper()
	a.seq++
	require.NoError(t, infra.Send(t.Context(), a.sqs, a.queues.InputURL, group, uuid.NewString(), body, nil))
}

func (a *consumerAPI) depth(t *testing.T, url string) int {
	t.Helper()
	n, err := infra.QueueDepth(t.Context(), a.sqs, url)
	require.NoError(t, err)
	return n
}

// drained waits until the input queue holds nothing, visible or not.
func (a *consumerAPI) drained(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return a.depth(t, a.queues.InputURL) == 0 }, 40*time.Second, 200*time.Millisecond, "the input queue to be emptied")
}

func (a *consumerAPI) balanceOf(t *testing.T, w apiWallet) int64 {
	t.Helper()
	var minor int64
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT balance_minor FROM wallets WHERE id = $1`, w.id).Scan(&minor))
	return minor
}

func (a *consumerAPI) debits(t *testing.T, w apiWallet) int {
	t.Helper()
	var n int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id).Scan(&n))
	return n
}

func (a *consumerAPI) metric(t *testing.T, series string) float64 {
	t.Helper()
	text, err := kc.Scrape(t.Context(), a.base)
	require.NoError(t, err)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, series+" ") {
			var v float64
			_, err := fmt.Sscanf(strings.TrimPrefix(line, series+" "), "%g", &v)
			require.NoError(t, err)
			return v
		}
	}
	t.Fatalf("no series %s in /metrics", series)
	return 0
}

func (a *consumerAPI) deadLetters(t *testing.T, want int) []infra.QueuedMessage {
	t.Helper()
	got, err := infra.ReceiveMessages(t.Context(), a.sqs, a.queues.DLQURL, want, 20*time.Second)
	require.NoError(t, err)
	require.Len(t, got, want, "messages in the dead-letter queue")
	return got
}

func TestAMessageIsHandledAndOnlyThenDeletedFromTheQueue(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")

	a.send(t, w.id, requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00"))
	a.drained(t)

	require.Equal(t, int64(7500), a.balanceOf(t, w))
	require.Equal(t, 1, a.debits(t, w))
	var completed bool
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL FROM inbox WHERE message_id = 'msg-1'`).Scan(&completed))
	require.True(t, completed)

	// The outcome is the one the HTTP endpoint reports for the same operation.
	status, _, answer := a.submit(t, a.client, a.provider, "provider-a:tx-1", operation(w, "tx-1", "BET", "25.00"))
	require.Equal(t, http.StatusOK, status)
	require.True(t, answer.IdempotentReplay, "HTTP finds what SQS did")
	require.Equal(t, "75.00", answer.Balance.Amount)
	require.Equal(t, 1, a.debits(t, w), "one movement for the operation sent both ways")
	require.EqualValues(t, 1, a.metric(t, `sqs_messages_handled_total{result="handled"}`))
	require.Equal(t, 0, a.depth(t, a.queues.DLQURL))
}

func TestTheSameOperationByHTTPFirstAndThenByMessageMovesMoneyOnce(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")
	status, _, _ := a.submit(t, a.client, a.provider, "provider-a:tx-1", operation(w, "tx-1", "BET", "25.00"))
	require.Equal(t, http.StatusOK, status)

	a.send(t, w.id, requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00"))
	a.drained(t)

	require.Equal(t, int64(7500), a.balanceOf(t, w))
	require.Equal(t, 1, a.debits(t, w))
	require.EqualValues(t, 1, a.metric(t, `sqs_messages_handled_total{result="duplicate"}`), "the message was a replay of the HTTP request")
}

func TestCopiesOfOneMessageSentWithDifferentQueueDeduplicationIdsMoveMoneyOnce(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")
	body := requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00")

	const copies = 12
	for range copies {
		a.send(t, w.id, body)
	}
	a.drained(t)

	require.Equal(t, int64(7500), a.balanceOf(t, w))
	require.Equal(t, 1, a.debits(t, w), "the application deduplicates, not the queue")
	var inbox int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM inbox`).Scan(&inbox))
	require.Equal(t, 1, inbox)
	require.EqualValues(t, copies, a.metric(t, "sqs_messages_received_total"))
	require.EqualValues(t, 1, a.metric(t, `sqs_messages_handled_total{result="handled"}`))
	require.EqualValues(t, copies-1, a.metric(t, `sqs_messages_handled_total{result="duplicate"}`))
	require.EqualValues(t, copies-1, a.metric(t, `wager_duplicates_total{type="message"}`))
}

func TestAnotherMessageForTheSameOperationIsAReplayNotASecondMovement(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")

	for i := range 5 {
		a.send(t, w.id, requestBody(fmt.Sprintf("msg-%d", i), w, "tx-1", "provider-a:tx-1", "BET", "25.00"))
	}
	a.drained(t)

	require.Equal(t, int64(7500), a.balanceOf(t, w))
	require.Equal(t, 1, a.debits(t, w))
	var inbox int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM inbox`).Scan(&inbox))
	require.Equal(t, 5, inbox)
}

func TestARefusalByARuleIsTerminalStoredAndTheMessageIsDeleted(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "10.00")

	a.send(t, w.id, requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00"))
	a.drained(t)

	require.Equal(t, int64(1000), a.balanceOf(t, w))
	var status, code string
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT status, failure_code FROM wager_transactions WHERE external_transaction_id = 'tx-1'`).Scan(&status, &code))
	require.Equal(t, "REJECTED", status)
	require.Equal(t, "INSUFFICIENT_FUNDS", code)
	require.Equal(t, 0, a.depth(t, a.queues.DLQURL), "a refusal is an answer, not a failure")
	require.Equal(t, http.StatusUnprocessableEntity, func() int {
		s, _, _ := a.submit(t, a.client, a.provider, "provider-a:tx-1", operation(w, "tx-1", "BET", "25.00"))
		return s
	}(), "and HTTP gets the same stored refusal")
}

func TestWhatCanNeverBeHandledGoesToTheDeadLetterQueueWithoutBlockingItsGroup(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")

	// All in one message group: each bad message is followed by a good one that must not wait.
	bad := map[string]string{
		"malformed":                    `{"messageId":`,
		"unknown type":                 strings.Replace(requestBody("msg-type", w, "tx-t", "provider-a:tx-t", "BET", "1.00"), "WagerTransactionRequested", "SomethingElse", 1),
		"opening":                      requestBody("msg-opening", w, "tx-o", "provider-a:tx-o", "OPENING", "1.00"),
		"invalid amount":               requestBody("msg-amount", w, "tx-a", "provider-a:tx-a", "BET", "1.5"),
		"zero bet":                     requestBody("msg-zero", w, "tx-z", "provider-a:tx-z", "BET", "0.00"),
		"a bet naming a reference":     strings.Replace(requestBody("msg-ref", w, "tx-ref", "provider-a:tx-ref", "BET", "1.00"), `"roundId"`, `"referenceExternalTransactionId":"tx-0","roundId"`, 1),
		"a refund without a reference": requestBody("msg-refund", w, "tx-r", "provider-a:tx-r", "REFUND", "1.00"),
	}
	reasons := map[string]string{}
	n := 0
	for name, body := range bad {
		a.send(t, w.id, body)
		n++
		a.send(t, w.id, requestBody(fmt.Sprintf("msg-good-%d", n), w, fmt.Sprintf("tx-good-%d", n), fmt.Sprintf("provider-a:tx-good-%d", n), "BET", "1.00"))
		reasons[name] = ""
	}
	a.drained(t)

	require.Equal(t, int64(10000-n*100), a.balanceOf(t, w), "every good message was handled")
	require.Equal(t, n, a.debits(t, w))

	letters := a.deadLetters(t, len(bad))
	seen := map[string]int{}
	for _, l := range letters {
		seen[l.Attributes["errorReason"]]++
		require.Equal(t, w.id, l.GroupID)
		require.NotEmpty(t, l.Attributes["errorDetail"])
		require.NotEmpty(t, l.Body, "the message travels as it came")
	}
	require.Equal(t, map[string]int{"malformed": 1, "unknown_type": 1, "invalid": 5}, seen)
	require.EqualValues(t, len(bad), a.metric(t, `sqs_messages_handled_total{result="dead_lettered"}`))
	require.EqualValues(t, 1, a.metric(t, `sqs_dead_letters_total{reason="malformed"}`))
	var stored int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM inbox WHERE message_id IN ('msg-type','msg-opening','msg-amount','msg-zero','msg-ref','msg-refund')`).Scan(&stored))
	require.Zero(t, stored, "a message that could not be handled leaves no trace but the dead letter")
}

func TestTheSameMessageIdWithOtherContentIsDeadLettered(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")
	a.send(t, w.id, requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00"))
	a.drained(t)

	a.send(t, w.id, requestBody("msg-1", w, "tx-2", "provider-a:tx-2", "BET", "10.00"))
	a.drained(t)

	require.Equal(t, int64(7500), a.balanceOf(t, w))
	letters := a.deadLetters(t, 1)
	require.Equal(t, "message_conflict", letters[0].Attributes["errorReason"])
}

func TestAnIdempotencyKeyReusedWithOtherContentIsDeadLettered(t *testing.T) {
	a := startConsumerAPI(t, nil)
	w := a.openFunded(t, "100.00")
	a.send(t, w.id, requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00"))
	a.send(t, w.id, requestBody("msg-2", w, "tx-1", "provider-a:tx-1", "BET", "30.00"))
	a.drained(t)

	require.Equal(t, int64(7500), a.balanceOf(t, w))
	require.Equal(t, "key_conflict", a.deadLetters(t, 1)[0].Attributes["errorReason"])
}

// While a wallet's lock is held elsewhere, handling its message runs out of time. That is
// a failure of the moment: the message stays, is tried again later, and works once the
// lock is gone, with nothing moved twice.
func TestAFailureOfTheMomentLeavesTheMessageToBeTriedAgainLater(t *testing.T) {
	a := startConsumerAPI(t, func(c *config.Consumer) { c.ProcessingTimeout = time.Second })
	w := a.openFunded(t, "100.00")

	owner := connect(t.Context(), t, a.ownerDSN)
	tx, err := owner.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), `SELECT id FROM wallets WHERE id = $1 FOR UPDATE`, w.id)
	require.NoError(t, err)

	a.send(t, w.id, requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00"))
	require.Eventually(t, func() bool { return a.metric(t, `sqs_messages_handled_total{result="retried"}`) >= 2 }, 30*time.Second, 200*time.Millisecond,
		"the message to be received and to fail more than once while the wallet is held")
	require.Equal(t, int64(10000), a.balanceOf(t, w))
	require.Equal(t, 1, a.depth(t, a.queues.InputURL), "kept in the queue, not lost and not dead-lettered")
	require.Equal(t, 0, a.depth(t, a.queues.DLQURL))

	require.NoError(t, tx.Rollback(t.Context()))
	a.drained(t)

	require.Equal(t, int64(7500), a.balanceOf(t, w))
	require.Equal(t, 1, a.debits(t, w))
	require.Equal(t, 0, a.depth(t, a.queues.DLQURL))
	require.Contains(t, a.logs.String(), `"visibilitySeconds"`)
	requireNoPayloadInLogs(t, a.logs.String())
}

func TestAMessageThatNeverWorksEndsInTheDeadLetterQueueByRedrive(t *testing.T) {
	a := startConsumerAPI(t, func(c *config.Consumer) { c.ProcessingTimeout = time.Second })
	w := a.openFunded(t, "100.00")
	owner := connect(t.Context(), t, a.ownerDSN)
	tx, err := owner.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	_, err = tx.Exec(t.Context(), `SELECT id FROM wallets WHERE id = $1 FOR UPDATE`, w.id)
	require.NoError(t, err)

	a.send(t, w.id, requestBody("msg-1", w, "tx-1", "provider-a:tx-1", "BET", "25.00"))

	letters, err := infra.ReceiveMessages(t.Context(), a.sqs, a.queues.DLQURL, 1, 60*time.Second)
	require.NoError(t, err)
	require.Len(t, letters, 1, "after three receives the queue's redrive moves it")
	require.Equal(t, int64(10000), a.balanceOf(t, w))
}

func requireNoPayloadInLogs(t *testing.T, logs string) {
	t.Helper()
	for _, text := range []string{"provider-a:tx-", `"amount"`, `"25.00"`} {
		require.NotContains(t, logs, text)
	}
}

// Many wallets, many messages, two workers racing: each wallet ends with exactly its own
// movements, in a total that adds up.
func TestManyMessagesOnManyWalletsAreEachAppliedOnce(t *testing.T) {
	a := startConsumerAPI(t, func(c *config.Consumer) { c.Workers = 4 })
	const wallets, perWallet = 6, 10
	var ws []apiWallet
	for range wallets {
		ws = append(ws, a.openFunded(t, "1000.00"))
	}
	for i := range perWallet {
		for j, w := range ws {
			body := requestBody(fmt.Sprintf("msg-%d-%d", j, i), w, fmt.Sprintf("tx-%d-%d", j, i), fmt.Sprintf("provider-a:tx-%d-%d", j, i), "BET", "1.00")
			a.send(t, w.id, body)
			if i%3 == 0 {
				a.send(t, w.id, body) // a copy now and then
			}
		}
	}
	a.drained(t)

	for _, w := range ws {
		require.Equal(t, int64(100000-perWallet*100), a.balanceOf(t, w))
		require.Equal(t, perWallet, a.debits(t, w))
		status, result := a.reconcile(t, a.client, a.internal, w.id)
		require.Equal(t, http.StatusOK, status)
		require.True(t, result.Consistent)
	}
	var transactions int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wager_transactions WHERE kind = 'BET'`).Scan(&transactions))
	require.Equal(t, wallets*perWallet, transactions)
}
