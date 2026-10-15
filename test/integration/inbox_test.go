//go:build integration

package integration

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
)

const hash64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func (s *schema) inbox(c *pgx.Conn, consumer, messageID, hash string, completed any) string {
	return s.exec(c, `INSERT INTO inbox (consumer_name, message_id, payload_hash, received_at, completed_at) VALUES ($1, $2, $3, $4, $5)`,
		consumer, messageID, hash, now, completed)
}

func TestInboxConstraints(t *testing.T) {
	s := newSchema(t)

	require.Empty(t, s.inbox(s.owner, "wager-transactions.fifo", "msg-1", hash64, nil))
	require.Empty(t, s.inbox(s.owner, "wager-transactions.fifo", "msg-2", hash64, now.Add(time.Second)), "completed after it was received")
	require.Empty(t, s.inbox(s.owner, "wager-transactions.fifo", "msg-3", hash64, now), "completed in the instant it was received")
	require.Empty(t, s.inbox(s.owner, "another-consumer", "msg-1", hash64, nil), "message ids are per consumer")

	require.Equal(t, uniqueViolation, s.inbox(s.owner, "wager-transactions.fifo", "msg-1", hash64, nil), "a message is recorded once per consumer")
	require.Equal(t, checkViolation, s.inbox(s.owner, "", "msg-9", hash64, nil))
	require.Equal(t, checkViolation, s.inbox(s.owner, "c", "", hash64, nil))
	require.Empty(t, s.inbox(s.owner, "c", strings.Repeat("m", 128), hash64, nil))
	require.Equal(t, checkViolation, s.inbox(s.owner, "c", strings.Repeat("m", 129), hash64, nil))
	for name, hash := range map[string]string{
		"too short": hash64[:63], "too long": hash64 + "0", "upper case": strings.ToUpper(hash64), "not hex": strings.Repeat("g", 64), "empty": "",
	} {
		require.Equal(t, checkViolation, s.inbox(s.owner, "c", "bad-"+name, hash, nil), name)
	}
	require.Equal(t, checkViolation, s.inbox(s.owner, "c", "early", hash64, now.Add(-time.Second)), "completed before it was received")
}

// The application records and completes messages; it cannot rewrite what a message was
// or forget that it was seen.
func TestTheApplicationRoleCanRecordAndCompleteButNotRewriteOrForget(t *testing.T) {
	s := newSchema(t)
	require.Empty(t, s.inbox(s.app, "c", "msg-1", hash64, nil))

	require.Empty(t, s.exec(s.app, `UPDATE inbox SET completed_at = $1 WHERE message_id = 'msg-1'`, now))
	require.Equal(t, insufficientPrivilege, s.exec(s.app, `UPDATE inbox SET payload_hash = $1 WHERE message_id = 'msg-1'`, strings.Repeat("a", 64)), "the content a message had")
	require.Equal(t, insufficientPrivilege, s.exec(s.app, `UPDATE inbox SET received_at = $1 WHERE message_id = 'msg-1'`, now))
	require.Equal(t, insufficientPrivilege, s.exec(s.app, `UPDATE inbox SET message_id = 'msg-2' WHERE message_id = 'msg-1'`))
	require.Equal(t, insufficientPrivilege, s.exec(s.app, `DELETE FROM inbox WHERE message_id = 'msg-1'`), "forgetting a message would let it be handled twice")
	require.Equal(t, insufficientPrivilege, s.exec(s.app, `TRUNCATE inbox`))

	var hash string
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT payload_hash FROM inbox WHERE message_id = 'msg-1'`).Scan(&hash))
	require.Equal(t, hash64, hash)
}

// --- through the real use case and the real database ----------------------------------

// fingerprintOf hashes every row of the tables with its row version (xmin), so that any
// write at all, even one that changes nothing, shows.
func (s *schema) fingerprintOf(t *testing.T, tables ...string) string {
	t.Helper()
	var parts []string
	for _, table := range tables {
		var sum string
		require.NoError(t, s.owner.QueryRow(s.ctx,
			`SELECT md5(coalesce(string_agg(r.xmin::text || r::text, '|' ORDER BY r::text), '')) FROM `+table+` r`).Scan(&sum))
		parts = append(parts, table+"="+sum)
	}
	return strings.Join(parts, " ")
}

func (e *wagerEnv) inboxRows(t *testing.T) int { return e.countWhere(t, "inbox", "true") }

func (e *wagerEnv) message(in processwager.Input, id string) processwager.Input {
	in.Message = &processwager.Message{Consumer: "wager-transactions.fifo", ID: id}
	return in
}

func TestAMessageIsRecordedWithTheMovementItCaused(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)

	out, err := e.uc.Execute(t.Context(), e.message(e.bet(w, "provider-a", "tx-1", "k-1", 2500), "msg-1"))
	require.NoError(t, err)
	require.False(t, out.MessageAlreadyHandled)

	var completed bool
	var hash string
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL, payload_hash FROM inbox WHERE message_id = 'msg-1'`).Scan(&completed, &hash))
	require.True(t, completed)
	require.Regexp(t, `^[0-9a-f]{64}$`, hash)
	balance, _ := e.balance(t, w.id)
	require.Equal(t, int64(7500), balance)
}

func TestAMessageDeliveredAgainChangesNothing(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	in := e.message(e.bet(w, "provider-a", "tx-1", "k-1", 2500), "msg-1")
	_, err := e.uc.Execute(t.Context(), in)
	require.NoError(t, err)
	before := e.owner.fingerprintOf(t, "wallets", "wager_transactions", "wallet_ledger_entries", "outbox", "inbox")

	out, err := e.uc.Execute(t.Context(), in)

	require.NoError(t, err)
	require.True(t, out.MessageAlreadyHandled)
	require.Equal(t, before, e.owner.fingerprintOf(t, "wallets", "wager_transactions", "wallet_ledger_entries", "outbox", "inbox"), "not a row touched")
	require.Equal(t, 1, e.rec.count(processwager.OutcomeDuplicateMessage))
}

func TestTheSameMessageIdWithOtherContentIsRefusedAndNothingIsStored(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	_, err := e.uc.Execute(t.Context(), e.message(e.bet(w, "provider-a", "tx-1", "k-1", 2500), "msg-1"))
	require.NoError(t, err)

	other := e.message(e.bet(w, "provider-a", "tx-2", "k-2", 100), "msg-1")
	_, err = e.uc.Execute(t.Context(), other)

	require.ErrorIs(t, err, processwager.ErrMessageContentConflict)
	balance, version := e.balance(t, w.id)
	require.Equal(t, int64(7500), balance)
	require.Equal(t, int64(2), version)
	require.Equal(t, 0, e.countWhere(t, "wager_transactions", "external_transaction_id = 'tx-2'"), "tx-2 was never stored")
}

func TestARefusedMessageIsRecordedAsHandled(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 1000)

	out, err := e.uc.Execute(t.Context(), e.message(e.bet(w, "provider-a", "tx-1", "k-1", 2500), "msg-1"))

	require.NoError(t, err)
	require.Equal(t, "REJECTED", string(out.Transaction.Status()))
	require.Equal(t, 1, e.countWhere(t, "inbox", "message_id = 'msg-1' AND completed_at IS NOT NULL"), "a Rejection is final, so the message is done")
}

func TestAnErrorLeavesNoRecordOfTheMessage(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	_, err := e.uc.Execute(t.Context(), e.message(e.bet(w, "provider-a", "tx-1", "k-1", 2500), "msg-0"))
	require.NoError(t, err)

	// The same idempotency key with other content: refused after the message was recorded.
	_, err = e.uc.Execute(t.Context(), e.message(e.bet(w, "provider-a", "tx-9", "k-1", 100), "msg-2"))

	require.ErrorIs(t, err, processwager.ErrIdempotencyKeyConflict)
	require.Equal(t, 0, e.countWhere(t, "inbox", "message_id = 'msg-2'"), "rolled back with the rest")
}

func TestOneMessageCopiedManyTimesAndHandledAtOnceIsAppliedOnce(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 100000)
	in := e.message(e.bet(w, "provider-a", "tx-1", "k-1", 2500), "msg-1")

	const copies = 40
	var wg sync.WaitGroup
	var fresh, already, failed atomic.Int64
	start := make(chan struct{})
	for range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := e.uc.Execute(t.Context(), in)
			switch {
			case err != nil:
				failed.Add(1)
			case out.MessageAlreadyHandled:
				already.Add(1)
			default:
				fresh.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Zero(t, failed.Load())
	require.EqualValues(t, 1, fresh.Load(), "exactly one copy does the work")
	require.EqualValues(t, copies-1, already.Load(), "the others wait for it and find the message handled")
	balance, version := e.balance(t, w.id)
	require.Equal(t, int64(97500), balance)
	require.Equal(t, int64(2), version)
	require.Equal(t, 1, e.inboxRows(t))
}

// Another message for the same operation is not the same message: the idempotency key
// answers it, and the inbox records both.
func TestTwoMessagesForOneOperationMoveMoneyOnce(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)

	var replays atomic.Int64
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := e.uc.Execute(t.Context(), e.message(e.bet(w, "provider-a", "tx-1", "k-1", 2500), fmt.Sprintf("msg-%d", i)))
			require.NoError(t, err)
			if out.IdempotentReplay {
				replays.Add(1)
			}
		}()
	}
	wg.Wait()

	balance, _ := e.balance(t, w.id)
	require.Equal(t, int64(7500), balance)
	require.EqualValues(t, 9, replays.Load())
	require.Equal(t, 10, e.inboxRows(t))
	require.Equal(t, 1, e.countWhere(t, "wallet_ledger_entries", "direction = 'DEBIT'"))
}
