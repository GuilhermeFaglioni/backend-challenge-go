//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
)

// A fast policy, so a test can watch an operation wait, try again and give up: six
// attempts or twelve seconds, gaps of one second doubling to two.
func fastPending(c *config.Config) {
	c.PendingReference = config.PendingReference{
		MaxAttempts: 6, TTL: 12 * time.Second, BackoffBase: time.Second, BackoffMax: 2 * time.Second, MaxErrors: 3,
		PollInterval: 50 * time.Millisecond, BatchSize: 10, Lease: 3 * time.Second,
	}
}

const resolvedWithin = 40 * time.Second

type storedState struct {
	Status, FailureCode string
	Attempts            int
	NextAttempt, Expiry *time.Time
	Created             time.Time
}

func (a walletAPI) stored(t *testing.T, externalID string) storedState {
	t.Helper()
	var s storedState
	var failure *string
	require.NoError(t, a.pool.QueryRow(t.Context(),
		`SELECT status, failure_code, attempts, next_attempt_at, expires_at, created_at FROM wager_transactions WHERE external_transaction_id = $1`, externalID).
		Scan(&s.Status, &failure, &s.Attempts, &s.NextAttempt, &s.Expiry, &s.Created))
	if failure != nil {
		s.FailureCode = *failure
	}
	return s
}

func (a walletAPI) waitForStatus(t *testing.T, externalID, status string) storedState {
	t.Helper()
	var s storedState
	require.Eventually(t, func() bool {
		s = a.stored(t, externalID)
		return s.Status == status
	}, resolvedWithin, 50*time.Millisecond, "%s to become %s", externalID, status)
	return s
}

func (a walletAPI) metricValue(t *testing.T, series string) float64 {
	t.Helper()
	return (&consumerAPI{walletAPI: a}).metric(t, series)
}

func TestAnOperationBeforeItsReferenceIsAcceptedAsWaitingAndAppliedOnceItArrives(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")

	waiting := a.requireWaiting(t, w, "refund-1", "REFUND", "30.00", "bet-1")

	s := a.stored(t, "refund-1")
	require.Equal(t, "PENDING_REFERENCE", s.Status)
	require.GreaterOrEqual(t, s.Attempts, 1, "the arrival is the first attempt")
	require.NotNil(t, s.NextAttempt)
	require.NotNil(t, s.Expiry)
	require.WithinDuration(t, s.Created.Add(12*time.Second), *s.Expiry, time.Millisecond)
	balance, version := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	require.Equal(t, int64(1), version, "a waiting operation touches neither the wallet nor the ledger")
	require.Equal(t, []string{"WagerTransactionPendingReference"}, a.eventTypes(t, w, waiting.TransactionID))

	status, _, _, tx := a.read(t, a.strict, a.provider, "/wagering/transactions/"+waiting.TransactionID)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "PENDING_REFERENCE", tx.Status)
	require.Nil(t, tx.Balance)
	require.Equal(t, "bet-1", tx.ReferenceExternalTransactionID)

	// Sending it again while it waits is the same wait.
	status, again := a.send(t, a.strict, w, "refund-1", "REFUND", "30.00", "bet-1")
	require.Equal(t, http.StatusAccepted, status)
	require.True(t, again.IdempotentReplay)
	require.Equal(t, waiting.TransactionID, again.TransactionID)

	// The Reference arrives, and the worker applies the operation exactly once.
	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")
	a.waitForStatus(t, "refund-1", "PROCESSED")
	balance, version = a.wallet(t, w)
	require.Equal(t, int64(10000), balance, "the stake came back")
	require.Equal(t, int64(3), version, "opening, bet, refund")
	require.Equal(t, []string{"WagerTransactionPendingReference", "WagerTransactionProcessed", "WalletBalanceChanged"}, a.eventTypes(t, w, waiting.TransactionID))
	var cause string
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT causation_id FROM outbox WHERE event_type = 'WagerTransactionProcessed' AND payload->>'transactionId' = $1`, waiting.TransactionID).Scan(&cause))
	require.Equal(t, waiting.TransactionID, cause, "the waiting operation is the cause of its own outcome")

	// Now the same request answers with the outcome.
	status, done := a.send(t, a.strict, w, "refund-1", "REFUND", "30.00", "bet-1")
	require.Equal(t, http.StatusOK, status)
	require.True(t, done.IdempotentReplay)
	require.Equal(t, "100.00", done.Balance.Amount)

	require.Eventually(t, func() bool { return a.metricValue(t, `pending_reference_resolutions_total{outcome="processed"}`) >= 1 }, 10*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool { return a.metricValue(t, "pending_references") == 0 }, 10*time.Second, 100*time.Millisecond, "nothing is waiting any more")
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestAWinNamingABetThatComesLaterIsCreditedOnceTheBetArrives(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	a.requireWaiting(t, w, "win-1", "WIN", "90.00", "bet-1")

	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")
	a.waitForStatus(t, "win-1", "PROCESSED")

	balance, _ := a.wallet(t, w)
	require.Equal(t, int64(16000), balance, "100 - 30 + 90")
	a.requireConsistent(t, w)
}

func TestAReferenceThatNeverComesEndsInARejectionWithItsEvent(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	waiting := a.requireWaiting(t, w, "refund-1", "REFUND", "30.00", "bet-never")

	s := a.waitForStatus(t, "refund-1", "REJECTED")

	require.Equal(t, "REFERENCE_NOT_FOUND", s.FailureCode, "it never appeared")
	require.GreaterOrEqual(t, s.Attempts, 2, "it was tried again before it gave up")
	require.Equal(t, []string{"WagerTransactionPendingReference", "WagerTransactionRejected"}, a.eventTypes(t, w, waiting.TransactionID))
	balance, version := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	require.Equal(t, int64(1), version)

	status, answer := a.send(t, a.strict, w, "refund-1", "REFUND", "30.00", "bet-never")
	require.Equal(t, http.StatusUnprocessableEntity, status, "and sending it again answers with the rejection")
	require.Equal(t, "REFERENCE_NOT_FOUND", answer.FailureCode)
	require.True(t, answer.IdempotentReplay)

	require.Eventually(t, func() bool { return a.metricValue(t, `pending_reference_resolutions_total{outcome="expired"}`) >= 1 }, 10*time.Second, 100*time.Millisecond)
	require.GreaterOrEqual(t, a.metricValue(t, `pending_reference_resolutions_total{outcome="waiting"}`), 1.0, "the retries are counted")
	a.requireConsistent(t, w)
	a.strict.requireNoViolations()
}

func TestAReferenceThatAppearedAndNeverCompletedIsNotProcessed(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	// The WIN waits for a bet that never comes; the refund names the WIN. The WIN gives up
	// first, and the refund finds a Reference that appeared and never completed.
	a.requireWaiting(t, w, "win-1", "WIN", "10.00", "bet-never")
	time.Sleep(200 * time.Millisecond)
	a.requireWaiting(t, w, "refund-1", "ROLLBACK", "10.00", "win-1")

	win := a.waitForStatus(t, "win-1", "REJECTED")
	refund := a.waitForStatus(t, "refund-1", "REJECTED")

	require.Equal(t, "REFERENCE_NOT_FOUND", win.FailureCode)
	require.Equal(t, "REFERENCE_NOT_PROCESSED", refund.FailureCode)
	balance, _ := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	a.requireConsistent(t, w)
}

func TestAReferenceFromAnotherProviderDoesNotExistForThisOne(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")

	body := operationRef(w, "refund-b", "REFUND", "30.00", "bet-1")
	body = replaceProvider(body, "provider-b")
	status, _, answer := a.submit(t, a.strict, a.providerB, "provider-b:refund-b", body)
	require.Equal(t, http.StatusAccepted, status)
	require.Equal(t, "PENDING_REFERENCE", answer.Status)

	s := a.waitForStatus(t, "refund-b", "REJECTED")

	require.Equal(t, "REFERENCE_NOT_FOUND", s.FailureCode, "provider-b never finds provider-a's bet, however long it waits")
	balance, _ := a.wallet(t, w)
	require.Equal(t, int64(7000), balance)
	a.requireConsistent(t, w)
}

func replaceProvider(body, provider string) string {
	return strings.Replace(body, `"provider-a"`, fmt.Sprintf("%q", provider), 1)
}

// Two reversals waiting for the same bet: when it comes, one takes effect and the other
// finds the bet already reversed.
func TestTwoWaitingReversalsOfOneBetHaveOneEffect(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	a.requireWaiting(t, w, "refund-1", "REFUND", "30.00", "bet-1")
	a.requireWaiting(t, w, "rollback-1", "ROLLBACK", "30.00", "bet-1")

	bet := a.mustProcess(t, w, "bet-1", "BET", "30.00", "")
	var refund, rollback storedState
	require.Eventually(t, func() bool {
		refund, rollback = a.stored(t, "refund-1"), a.stored(t, "rollback-1")
		return refund.Status != "PENDING_REFERENCE" && rollback.Status != "PENDING_REFERENCE"
	}, resolvedWithin, 50*time.Millisecond)

	// Which of the two goes first is up to the worker; the other finds the bet reversed.
	statuses := []string{refund.Status, rollback.Status}
	require.ElementsMatch(t, []string{"PROCESSED", "REJECTED"}, statuses)
	loser := refund
	if rollback.Status == "REJECTED" {
		loser = rollback
	}
	require.Equal(t, "REFERENCE_ALREADY_REVERSED", loser.FailureCode)
	require.Equal(t, 1, a.reversalsOf(t, bet.TransactionID))
	balance, _ := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	a.requireConsistent(t, w)
}

// --- an acceptance that never finished -----------------------------------------------------

// leaveUnfinished commits an operation as PENDING, which a request never does: the state an
// acceptance would leave behind if it died half way.
func (a walletAPI) leaveUnfinished(t *testing.T, w apiWallet, kind wager.Kind, externalID string, minor int64, reference string) uuid.UUID {
	t.Helper()
	player := uuid.MustParse(w.player)
	amount, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	request, err := wager.NewRequest(wager.RequestParams{
		ProviderID: "provider-a", ExternalTransactionID: externalID, PlayerID: player, WalletID: uuid.MustParse(w.id),
		RoundID: "round-1", GameID: "game-1", Kind: kind, Amount: amount, ReferenceExternalTransactionID: reference,
	})
	require.NoError(t, err)
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request, IdempotencyKey: "provider-a:" + externalID, CorrelationID: "corr-orphan", At: time.Now()})
	require.NoError(t, err)
	uow := postgres.NewUnitOfWork(a.pool)
	require.NoError(t, uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
		claimed, err := repos.Transactions().Claim(ctx, tx)
		require.True(t, claimed)
		return err
	}))
	return tx.ID()
}

func TestAnAcceptanceThatNeverFinishedIsFinishedByTheWorker(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")
	bet := a.leaveUnfinished(t, w, wager.KindBet, "bet-orphan", 10_00, "")
	refund := a.leaveUnfinished(t, w, wager.KindRefund, "refund-orphan", 30_00, "bet-1")
	require.NotEqual(t, bet, refund)

	a.waitForStatus(t, "bet-orphan", "PROCESSED")
	a.waitForStatus(t, "refund-orphan", "PROCESSED")

	balance, version := a.wallet(t, w)
	require.Equal(t, int64(9000), balance, "100 - 30 - 10 + 30")
	require.Equal(t, int64(4), version, "each exactly once")
	require.Equal(t, 2, a.countEvents(t, "WagerTransactionProcessed", "causation_id IS NOT NULL AND payload->>'externalTransactionId' IN ('bet-orphan','refund-orphan')"))
	a.requireConsistent(t, w)
}

func (a walletAPI) countEvents(t *testing.T, eventType, where string) int {
	t.Helper()
	var n int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE event_type = $1 AND `+where, eventType).Scan(&n))
	return n
}

// --- through the queue ---------------------------------------------------------------------

func TestAReversalSentBeforeItsBetByMessageIsAppliedOnceTheBetArrives(t *testing.T) {
	a := startConsumerAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")

	a.send(t, w.id, messageRef("REFUND", "refund", w, "30.00", "tx-bet"))
	a.drained(t) // the message is concluded as soon as the wait is stored
	waiting := a.stored(t, "tx-refund")
	require.Equal(t, "PENDING_REFERENCE", waiting.Status)
	require.Equal(t, 0, a.depth(t, a.queues.DLQURL))
	var completed bool
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT completed_at IS NOT NULL FROM inbox WHERE message_id = 'msg-refund'`).Scan(&completed))
	require.True(t, completed, "the inbox is completed once the pending state is stored")

	a.send(t, w.id, messageFor("BET", "bet", w, "30.00"))
	a.drained(t)
	a.waitForStatus(t, "tx-refund", "PROCESSED")

	require.Equal(t, int64(10000), a.balanceOf(t, w))
	require.Equal(t, 1, a.debits(t, w))
	var credits int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'`, w.id).Scan(&credits))
	require.Equal(t, 2, credits, "the opening and the refund, once")
	a.requireConsistent(t, w)
}

func TestACopyOfAWaitingMessageIsRecognisedAndDoesNotWaitTwice(t *testing.T) {
	a := startConsumerAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	body := messageRef("REFUND", "refund", w, "30.00", "tx-bet")

	for range 4 {
		a.send(t, w.id, body)
	}
	a.drained(t)

	var waiting int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wager_transactions WHERE kind = 'REFUND'`).Scan(&waiting))
	require.Equal(t, 1, waiting)
	require.Equal(t, 1, a.countEvents(t, "WagerTransactionPendingReference", "true"))
}

// --- the reservation ---------------------------------------------------------------------

type pendingFixture struct {
	s     *schema
	store *postgres.PendingStore
	wal   uuid.UUID
	plr   uuid.UUID
}

func newPendingFixture(t *testing.T) pendingFixture {
	t.Helper()
	s := newSchema(t)
	w, p := s.wallet(t)
	return pendingFixture{s: s, store: postgres.NewPendingStore(appPool(s.ctx, t, s.appDSN)), wal: w, plr: p}
}

// waiting inserts a PENDING_REFERENCE operation whose next try is the given instant,
// relative to the database's clock.
func (f pendingFixture) waiting(t *testing.T, name string, nextIn time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.Empty(t, f.s.exec(f.s.owner, `INSERT INTO wager_transactions
		(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id,
		 amount_minor, currency, reference_external_transaction_id, status, attempts, next_attempt_at, expires_at, created_at, updated_at)
		VALUES ($1, 'EXTERNAL', 'REFUND', 'p', $2, $2, 'h', $3, $3, $4, 100, 'BRL', 'x', 'PENDING_REFERENCE', 1,
		        now() + make_interval(secs => $5), now() + interval '1 hour', now(), now())`, id, name, f.wal, f.plr, nextIn.Seconds()))
	return id
}

func idsOf(items []application.PendingItem) []uuid.UUID {
	out := make([]uuid.UUID, len(items))
	for i, item := range items {
		out[i] = item.TransactionID
	}
	return out
}

func TestOnlyWhatIsDueAndFreeIsReserved(t *testing.T) {
	f := newPendingFixture(t)
	due := f.waiting(t, "due", -time.Second)
	f.waiting(t, "later", time.Hour)

	first, err := f.store.Reserve(t.Context(), 10, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{due}, idsOf(first), "an operation whose try is not due is not reserved")
	require.Equal(t, f.wal, first[0].WalletID)

	again, err := f.store.Reserve(t.Context(), 10, 30*time.Second)
	require.NoError(t, err)
	require.Empty(t, again, "and a reserved one is not reserved again")

	require.NoError(t, f.store.Release(t.Context(), []uuid.UUID{due}))
	released, err := f.store.Reserve(t.Context(), 10, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{due}, idsOf(released), "until its lease is given back")
}

func TestALeaseThatRanOutMakesTheOperationAvailableAgain(t *testing.T) {
	f := newPendingFixture(t)
	due := f.waiting(t, "due", -time.Second)

	first, err := f.store.Reserve(t.Context(), 10, time.Second)
	require.NoError(t, err)
	require.Len(t, first, 1)

	require.Eventually(t, func() bool {
		again, err := f.store.Reserve(t.Context(), 10, time.Second)
		return err == nil && len(again) == 1 && again[0].TransactionID == due
	}, 10*time.Second, 100*time.Millisecond, "the holder died, so the lease runs out and another worker takes it")
}

func TestTheOldestDueOperationsComeFirstAndOnlyTheLimit(t *testing.T) {
	f := newPendingFixture(t)
	third := f.waiting(t, "c", -time.Second)
	first := f.waiting(t, "a", -time.Hour)
	second := f.waiting(t, "b", -time.Minute)

	got, err := f.store.Reserve(t.Context(), 2, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{first, second}, idsOf(got))
	rest, err := f.store.Reserve(t.Context(), 2, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{third}, idsOf(rest))
}

func TestAnUnfinishedAcceptanceIsDueAtOnceAndADecidedOneNever(t *testing.T) {
	f := newPendingFixture(t)
	orphan := uuid.New()
	require.Empty(t, f.s.exec(f.s.owner, `INSERT INTO wager_transactions
		(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id, amount_minor, currency, status, created_at, updated_at)
		VALUES ($1, 'EXTERNAL', 'BET', 'p', 'orphan', 'orphan', 'h', $2, $2, $3, 100, 'BRL', 'PENDING', now(), now())`, orphan, f.wal, f.plr))
	f.s.external(t, f.wal, f.plr, "p", "k-done", "e-done", "BET", 100, "PROCESSED")
	f.s.external(t, f.wal, f.plr, "p", "k-rej", "e-rej", "BET", 100, "REJECTED")

	got, err := f.store.Reserve(t.Context(), 10, 30*time.Second)

	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{orphan}, idsOf(got))
}

// Workers reserving at once never get the same operation, and between them they get all.
func TestWorkersReservingAtOnceNeverTakeTheSameOperation(t *testing.T) {
	f := newPendingFixture(t)
	want := map[uuid.UUID]bool{}
	for i := range 60 {
		want[f.waiting(t, fmt.Sprintf("op-%d", i), -time.Second)] = true
	}

	var mu sync.Mutex
	got := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				items, err := f.store.Reserve(t.Context(), 5, time.Minute)
				if err != nil {
					t.Errorf("reserve: %v", err)
					return
				}
				if len(items) == 0 {
					return
				}
				mu.Lock()
				for _, item := range items {
					got[item.TransactionID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	require.Len(t, got, len(want), "all of them were taken")
	for id, n := range got {
		require.Equal(t, 1, n, "and %s by one worker only", id)
	}
}

func TestTheStatsCountWhatWaitsAndHowLongTheOldestHas(t *testing.T) {
	f := newPendingFixture(t)
	stats, err := f.store.Stats(t.Context())
	require.NoError(t, err)
	require.Equal(t, application.PendingStats{}, stats)

	f.waiting(t, "a", time.Hour)
	f.waiting(t, "b", time.Hour)
	require.NoError(t, func() error {
		_, err := f.s.owner.Exec(f.s.ctx, `UPDATE wager_transactions SET created_at = now() - interval '90 seconds' WHERE external_transaction_id = 'a'`)
		return err
	}())

	stats, err = f.store.Stats(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, stats.Waiting)
	require.InDelta(t, 90, stats.Oldest.Seconds(), 3)
}

// --- the schema -------------------------------------------------------------------------

func TestAWaitingOperationNeedsItsSchedule(t *testing.T) {
	s := newSchema(t)
	wal, plr := s.wallet(t)
	insert := func(mutate string) string {
		id := uuid.New()
		cols := map[string]string{
			"reference_external_transaction_id": "'x'", "attempts": "1", "next_attempt_at": "now()", "expires_at": "now() + interval '1 hour'",
		}
		switch mutate {
		case "no reference":
			cols["reference_external_transaction_id"] = "NULL"
		case "no attempts":
			cols["attempts"] = "0"
		case "no next attempt":
			cols["next_attempt_at"] = "NULL"
		case "no expiry":
			cols["expires_at"] = "NULL"
		case "expiry before arrival":
			cols["expires_at"] = "now() - interval '1 hour'"
		case "negative attempts":
			cols["attempts"] = "-1"
		}
		return s.exec(s.owner, fmt.Sprintf(`INSERT INTO wager_transactions
			(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id,
			 amount_minor, currency, reference_external_transaction_id, status, attempts, next_attempt_at, expires_at, created_at, updated_at)
			VALUES ($1, 'EXTERNAL', 'REFUND', 'p', $1::uuid::text, $1::uuid::text, 'h', $2, $2, $3, 100, 'BRL', %s, 'PENDING_REFERENCE', %s, %s, %s, now(), now())`,
			cols["reference_external_transaction_id"], cols["attempts"], cols["next_attempt_at"], cols["expires_at"]), id, wal, plr)
	}
	require.Empty(t, insert(""))
	for _, broken := range []string{"no reference", "no attempts", "no next attempt", "no expiry", "expiry before arrival", "negative attempts"} {
		require.Equal(t, checkViolation, insert(broken), broken)
	}
}

func TestOnlyAWaitingOperationNeedsASchedule(t *testing.T) {
	s := newSchema(t)
	wal, plr := s.wallet(t)
	// An ordinary operation, and a decided one, carry no schedule.
	s.external(t, wal, plr, "p", "k1", "e1", "BET", 100, "PROCESSED")
	s.external(t, wal, plr, "p", "k2", "e2", "BET", 100, "REJECTED")
	var attempts int
	require.NoError(t, s.owner.QueryRow(s.ctx, `SELECT attempts FROM wager_transactions WHERE external_transaction_id = 'e1'`).Scan(&attempts))
	require.Zero(t, attempts, "the default of an operation that never waited")
}

func TestTheWorkersScanIsServedByItsIndexes(t *testing.T) {
	s := newSchema(t)
	var names []string
	rows, err := s.owner.Query(s.ctx, `SELECT indexname FROM pg_indexes WHERE tablename = 'wager_transactions' ORDER BY indexname`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.Contains(t, names, "wager_transactions_pending_due")
	require.Contains(t, names, "wager_transactions_unfinished")
}

// A worker never waits for a row another holds: it takes the others. (Without SKIP LOCKED
// it would sit behind the lock, and a worker busy with one operation would hold back every
// other worker.)
func TestAWorkerDoesNotWaitForARowAnotherTransactionHolds(t *testing.T) {
	f := newPendingFixture(t)
	held := f.waiting(t, "held", -time.Second)
	free := f.waiting(t, "free", -time.Second)

	holder, err := f.s.owner.Begin(f.s.ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	_, err = holder.Exec(f.s.ctx, `SELECT 1 FROM wager_transactions WHERE id = $1 FOR UPDATE`, held)
	require.NoError(t, err)

	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := f.store.Reserve(ctx, 10, 30*time.Second)

	require.NoError(t, err, "it did not sit behind the lock")
	require.Less(t, time.Since(started), 3*time.Second)
	require.Equal(t, []uuid.UUID{free}, idsOf(got))
}

// --- reading and recording a wait with the repository ----------------------------------------

func TestAnUndecidedOperationIsReadWithItsScheduleAndADecidedOneIsNot(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	id := a.leaveUnfinished(t, w, wager.KindRefund, "refund-1", 30_00, "bet-1")
	uow := postgres.NewUnitOfWork(a.pool)

	read := func() (wager.Transaction, application.PendingSchedule, bool) {
		var tx wager.Transaction
		var schedule application.PendingSchedule
		var found bool
		require.NoError(t, uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
			var err error
			tx, schedule, found, err = repos.Transactions().ReadPending(ctx, id)
			return err
		}))
		return tx, schedule, found
	}

	// Stopped here by the fast worker? It would already have decided it; read first.
	tx, schedule, found := read()
	if found {
		require.Equal(t, wager.StatusPending, tx.Status(), "an acceptance that never finished")
		require.Zero(t, schedule.Attempts)
	}

	a.waitForStatus(t, "refund-1", "PENDING_REFERENCE")
	tx, schedule, found = read()
	require.True(t, found)
	require.Equal(t, wager.StatusPendingReference, tx.Status())
	require.GreaterOrEqual(t, schedule.Attempts, 1)
	require.False(t, schedule.NextAttemptAt.IsZero())
	require.False(t, schedule.ExpiresAt.IsZero())

	a.waitForStatus(t, "refund-1", "REJECTED")
	_, _, found = read()
	require.False(t, found, "once decided it is no longer waiting")
	_, _, found = func() (wager.Transaction, application.PendingSchedule, bool) {
		var f bool
		require.NoError(t, uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
			var err error
			_, _, f, err = repos.Transactions().ReadPending(ctx, uuid.New())
			return err
		}))
		return wager.Transaction{}, application.PendingSchedule{}, f
	}()
	require.False(t, found, "and one that does not exist is not found either")
}

// --- an operation that cannot be tried at all ----------------------------------------------

// A waiting operation whose row the application cannot read (its hash is not the hash of
// what it holds, which the application never writes) can never be tried. It is counted, held
// back longer each time, and after the limit it ends as FAILED, for audit, instead of being
// tried forever. A healthy operation next to it is not affected.
func TestAnOperationThatKeepsFailingEndsAsFailedForAudit(t *testing.T) {
	a := startWalletAPIWith(t, fastPending)
	w := a.openFunded(t, "100.00")
	owner := connect(t.Context(), t, a.ownerDSN)
	_, err := owner.Exec(t.Context(), `INSERT INTO wager_transactions
		(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id,
		 round_id, game_id, amount_minor, currency, reference_external_transaction_id, status, attempts, next_attempt_at, expires_at, created_at, updated_at)
		VALUES (gen_random_uuid(), 'EXTERNAL', 'REFUND', 'provider-a', 'broken-1', 'provider-a:broken-1', 'not-the-hash', $1, $1, $2,
		        'round-1', 'game-1', 100, 'BRL', 'bet-x', 'PENDING_REFERENCE', 1, now(), now() + interval '1 hour', now(), now())`, w.id, w.player)
	require.NoError(t, err)
	a.requireWaiting(t, w, "refund-ok", "REFUND", "30.00", "bet-1")

	s := a.waitForStatus(t, "broken-1", "FAILED")

	require.Equal(t, "INTERNAL_PERMANENT_FAILURE", s.FailureCode)
	var errorCount int
	var lastError string
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT error_count, last_error FROM wager_transactions WHERE external_transaction_id = 'broken-1'`).Scan(&errorCount, &lastError))
	require.Equal(t, 3, errorCount, "failed at the configured limit, not before")
	require.Contains(t, lastError, "is not a valid transaction", "the reason is kept for whoever audits it")
	require.Eventually(t, func() bool { return a.metricValue(t, `pending_reference_resolutions_total{outcome="failed"}`) == 1 }, 10*time.Second, 100*time.Millisecond)
	require.EqualValues(t, 3, a.metricValue(t, `pending_reference_resolutions_total{outcome="error"}`))
	require.Contains(t, a.logs.String(), "kept failing and was ended as FAILED")

	// The healthy one still works.
	a.mustProcess(t, w, "bet-1", "BET", "30.00", "")
	a.waitForStatus(t, "refund-ok", "PROCESSED")
	balance, _ := a.wallet(t, w)
	require.Equal(t, int64(10000), balance)
	a.requireConsistent(t, w)
}

func TestAnErrorIsCountedAndHoldsTheOperationBackLongerEachTime(t *testing.T) {
	f := newPendingFixture(t)
	id := f.waiting(t, "op", -time.Second)

	var holds []float64
	for range 4 {
		n, err := f.store.RecordError(t.Context(), id, "a defect", 2*time.Second, 5*time.Second)
		require.NoError(t, err)
		var hold float64
		require.NoError(t, f.s.owner.QueryRow(f.s.ctx, `SELECT extract(epoch FROM locked_until - now())::float8 FROM wager_transactions WHERE id = $1`, id).Scan(&hold))
		holds = append(holds, hold)
		require.Equal(t, len(holds), n)
	}
	for i, want := range []float64{2, 4, 5, 5} {
		require.InDelta(t, want, holds[i], 0.5, "after error %d", i+1)
	}
	got, err := f.store.Reserve(t.Context(), 10, time.Minute)
	require.NoError(t, err)
	require.Empty(t, got, "a held-back operation is not reserved")

	var count int
	var reason string
	require.NoError(t, f.s.owner.QueryRow(f.s.ctx, `SELECT error_count, last_error FROM wager_transactions WHERE id = $1`, id).Scan(&count, &reason))
	require.Equal(t, 4, count)
	require.Equal(t, "a defect", reason)

	long, err := f.store.RecordError(t.Context(), id, strings.Repeat("x", 2000)+"\xff", time.Second, time.Second)
	require.NoError(t, err)
	require.Equal(t, 5, long, "a long or broken reason is cut, not refused")
}

func TestFailingEndsOnlyAnUndecidedOperation(t *testing.T) {
	f := newPendingFixture(t)
	id := f.waiting(t, "op", -time.Second)

	failed, err := f.store.Fail(t.Context(), id)
	require.NoError(t, err)
	require.True(t, failed)
	var status, code string
	var processed bool
	require.NoError(t, f.s.owner.QueryRow(f.s.ctx, `SELECT status, failure_code, processed_at IS NOT NULL FROM wager_transactions WHERE id = $1`, id).Scan(&status, &code, &processed))
	require.Equal(t, "FAILED", status)
	require.Equal(t, "INTERNAL_PERMANENT_FAILURE", code)
	require.True(t, processed)

	again, err := f.store.Fail(t.Context(), id)
	require.NoError(t, err)
	require.False(t, again, "a terminal operation is not touched again")
	n, err := f.store.RecordError(t.Context(), id, "late", time.Second, time.Second)
	require.NoError(t, err)
	require.Zero(t, n, "nor counted")
}

func TestTheErrorCountAndTheReasonAreChecked(t *testing.T) {
	f := newPendingFixture(t)
	id := f.waiting(t, "op", time.Hour)
	require.Equal(t, checkViolation, f.s.exec(f.s.owner, `UPDATE wager_transactions SET error_count = -1 WHERE id = $1`, id))
	require.Equal(t, checkViolation, f.s.exec(f.s.owner, `UPDATE wager_transactions SET last_error = '' WHERE id = $1`, id))
	require.Equal(t, checkViolation, f.s.exec(f.s.owner, `UPDATE wager_transactions SET last_error = repeat('x', 501) WHERE id = $1`, id))
	require.Empty(t, f.s.exec(f.s.owner, `UPDATE wager_transactions SET last_error = repeat('x', 500) WHERE id = $1`, id))
}
