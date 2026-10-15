package resolvepending_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/resolvepending"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

var (
	t0     = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	player = uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7")
	wid    = uuid.MustParse("3fa85f64-5717-4562-b3fc-2c963f66afa6")
	policy = application.PendingPolicy{MaxAttempts: 4, TTL: 15 * time.Minute, BackoffBase: 2 * time.Second, BackoffMax: time.Minute, Jitter: func() float64 { return 0.5 }}
)

func brl(minor int64) money.Money {
	m, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		panic(err)
	}
	return m
}

// --- a small in-memory database ---------------------------------------------------------

type db struct {
	wallet   wallet.Wallet
	txs      map[uuid.UUID]wager.Transaction
	byExtID  map[string]wager.Transaction
	sched    map[uuid.UUID]application.PendingSchedule
	entries  []wallet.LedgerEntry
	events   []event.Event
	finished []wager.Transaction
	calls    []string
	failAt   string
}

func (d *db) step(name string) error {
	d.calls = append(d.calls, name)
	if d.failAt == name {
		return errors.New("injected")
	}
	return nil
}

func (d *db) Do(ctx context.Context, fn func(context.Context, application.Repositories) error) error {
	return fn(ctx, d)
}
func (d *db) Wallets() application.WalletRepository           { return (*walletRepo)(d) }
func (d *db) Transactions() application.TransactionRepository { return (*txRepo)(d) }
func (d *db) Ledger() application.LedgerRepository            { return (*ledgerRepo)(d) }
func (d *db) Outbox() application.OutboxRepository            { return (*outboxRepo)(d) }
func (d *db) Inbox() application.InboxRepository              { panic("not used") }

type (
	walletRepo (db)
	txRepo     (db)
	ledgerRepo (db)
	outboxRepo (db)
)

func (r *walletRepo) Insert(context.Context, wallet.Wallet) error { panic("not used") }
func (r *walletRepo) Lock(context.Context, uuid.UUID) (wallet.Wallet, error) {
	d := (*db)(r)
	if err := d.step("lock"); err != nil {
		return wallet.Wallet{}, err
	}
	return d.wallet, nil
}
func (r *walletRepo) Update(_ context.Context, _, after wallet.Wallet) error {
	d := (*db)(r)
	d.calls = append(d.calls, "update")
	d.wallet = after
	return nil
}
func (r *txRepo) InsertOpening(context.Context, wager.Transaction) error { panic("not used") }
func (r *txRepo) Find(context.Context, string, string, string) (application.ExistingTransactions, error) {
	panic("not used")
}
func (r *txRepo) Claim(context.Context, wager.Transaction) (bool, error) { panic("not used") }
func (r *txRepo) ResolveReference(_ context.Context, _, ext string) (wager.Reference, error) {
	d := (*db)(r)
	if err := d.step("reference"); err != nil {
		return wager.Reference{}, err
	}
	ref, ok := d.byExtID[ext]
	if !ok {
		return wager.Reference{}, nil
	}
	reversed := false
	for _, t := range d.txs {
		if t.External().ReferenceTransactionID == ref.ID() && t.Status() == wager.StatusProcessed && (t.Kind() == wager.KindRefund || t.Kind() == wager.KindRollback) {
			reversed = true
		}
	}
	return wager.Reference{Found: true, Transaction: ref, Reversed: reversed}, nil
}
func (r *txRepo) ReadPending(_ context.Context, id uuid.UUID) (wager.Transaction, application.PendingSchedule, bool, error) {
	d := (*db)(r)
	if err := d.step("read"); err != nil {
		return wager.Transaction{}, application.PendingSchedule{}, false, err
	}
	t, ok := d.txs[id]
	if !ok || (t.Status() != wager.StatusPending && t.Status() != wager.StatusPendingReference) {
		return wager.Transaction{}, application.PendingSchedule{}, false, nil
	}
	return t, d.sched[id], true, nil
}
func (r *txRepo) Await(_ context.Context, t wager.Transaction, s application.PendingSchedule) error {
	d := (*db)(r)
	d.calls = append(d.calls, "await")
	d.txs[t.ID()] = t
	d.sched[t.ID()] = s
	return nil
}
func (r *txRepo) Finish(_ context.Context, t wager.Transaction) error {
	d := (*db)(r)
	d.calls = append(d.calls, "finish")
	d.txs[t.ID()] = t
	d.byExtID[t.External().ExternalTransactionID] = t
	d.finished = append(d.finished, t)
	return nil
}
func (r *ledgerRepo) Append(_ context.Context, e wallet.LedgerEntry) error {
	d := (*db)(r)
	d.calls = append(d.calls, "ledger")
	d.entries = append(d.entries, e)
	return nil
}
func (r *outboxRepo) Add(_ context.Context, e event.Event) error {
	d := (*db)(r)
	d.calls = append(d.calls, "outbox")
	d.events = append(d.events, e)
	return nil
}

type recorder struct{ outcomes []resolvepending.Outcome }

func (r *recorder) Resolved(o resolvepending.Outcome) { r.outcomes = append(r.outcomes, o) }

type fixture struct {
	db  *db
	rec *recorder
	uc  *resolvepending.UseCase
	now time.Time
}

func newFixture(t *testing.T, balance int64) *fixture {
	t.Helper()
	w, err := wallet.Rehydrate(wallet.State{ID: wid, PlayerID: player, Balance: brl(balance), Version: 2, LastTransactionID: uuid.New(), CreatedAt: t0, UpdatedAt: t0})
	require.NoError(t, err)
	f := &fixture{
		db:  &db{wallet: w, txs: map[uuid.UUID]wager.Transaction{}, byExtID: map[string]wager.Transaction{}, sched: map[uuid.UUID]application.PendingSchedule{}},
		rec: &recorder{}, now: t0,
	}
	f.uc = resolvepending.New(resolvepending.Deps{
		UnitOfWork: f.db, Recorder: f.rec, Now: func() time.Time { return f.now }, NewID: uuid.New, Policy: policy,
	})
	return f
}

func request(t *testing.T, p wager.RequestParams) wager.Request {
	t.Helper()
	r, err := wager.NewRequest(p)
	require.NoError(t, err)
	return r
}

func params(kind wager.Kind, externalID string, minor int64, reference string) wager.RequestParams {
	return wager.RequestParams{
		ProviderID: "provider-a", ExternalTransactionID: externalID, PlayerID: player, WalletID: wid, RoundID: "round-1", GameID: "game-1",
		Kind: kind, Amount: brl(minor), ReferenceExternalTransactionID: reference,
	}
}

func newTx(t *testing.T, p wager.RequestParams, at time.Time) wager.Transaction {
	t.Helper()
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request(t, p), IdempotencyKey: "key-" + p.ExternalTransactionID, CorrelationID: "corr-1", At: at})
	require.NoError(t, err)
	return tx
}

// waiting stores an operation that has been waiting since t0 with the given tries behind it.
func (f *fixture) waiting(t *testing.T, p wager.RequestParams, attempts int) wager.Transaction {
	t.Helper()
	tx, err := newTx(t, p, t0).AwaitReference(t0)
	require.NoError(t, err)
	f.db.txs[tx.ID()] = tx
	f.db.byExtID[p.ExternalTransactionID] = tx
	f.db.sched[tx.ID()] = application.PendingSchedule{Attempts: attempts, NextAttemptAt: t0, ExpiresAt: t0.Add(15 * time.Minute)}
	return tx
}

// processedBet stores a BET that went through, the Reference a reversal can name.
func (f *fixture) processedBet(t *testing.T, externalID string, minor int64) wager.Transaction {
	t.Helper()
	tx, err := newTx(t, params(wager.KindBet, externalID, minor, ""), t0).Process(wager.Result{Balance: brl(1), WalletVersion: 2}, t0)
	require.NoError(t, err)
	f.db.txs[tx.ID()] = tx
	f.db.byExtID[externalID] = tx
	return tx
}

func (f *fixture) item(tx wager.Transaction) application.PendingItem {
	return application.PendingItem{TransactionID: tx.ID(), WalletID: wid}
}

func (f *fixture) try(t *testing.T, tx wager.Transaction) resolvepending.Result {
	t.Helper()
	r, err := f.uc.Execute(t.Context(), f.item(tx))
	require.NoError(t, err)
	return r
}

func types(events []event.Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type())
	}
	return out
}

// --- tries --------------------------------------------------------------------------------

func TestAnOperationIsAppliedWhenItsReferenceHasArrived(t *testing.T) {
	f := newFixture(t, 10000)
	tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "bet-1"), 1)
	bet := f.processedBet(t, "bet-1", 2500)
	f.now = t0.Add(5 * time.Second)

	r := f.try(t, tx)

	require.Equal(t, resolvepending.OutcomeProcessed, r.Outcome)
	require.Equal(t, 2, r.Attempts)
	require.Equal(t, []string{"lock", "read", "reference", "update", "ledger", "finish", "outbox", "outbox"}, f.db.calls,
		"the wallet is locked first, then the operation is read again")
	require.Equal(t, int64(12500), f.db.wallet.Balance().Minor())
	done := f.db.txs[tx.ID()]
	require.Equal(t, wager.StatusProcessed, done.Status())
	require.Equal(t, bet.ID(), done.External().ReferenceTransactionID)
	require.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, types(f.db.events))
	require.Equal(t, tx.ID().String(), f.db.events[0].CausationID(), "the cause of the events is the waiting operation")
	require.Equal(t, "corr-1", f.db.events[0].CorrelationID(), "and the correlation is the one it arrived with")
	require.Equal(t, []resolvepending.Outcome{resolvepending.OutcomeProcessed}, f.rec.outcomes)
}

func TestAnOperationStillWithoutItsReferenceWaitsForTheNextTry(t *testing.T) {
	f := newFixture(t, 10000)
	tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "bet-1"), 1)
	f.now = t0.Add(3 * time.Second)

	r := f.try(t, tx)

	require.Equal(t, resolvepending.OutcomeWaiting, r.Outcome)
	require.Equal(t, []string{"lock", "read", "reference", "await"}, f.db.calls, "nothing but the schedule is written")
	require.Equal(t, wager.StatusPendingReference, f.db.txs[tx.ID()].Status())
	s := f.db.sched[tx.ID()]
	require.Equal(t, 2, s.Attempts)
	require.Equal(t, f.now.Add(4*time.Second), s.NextAttemptAt, "the gap doubles: 2s, then 4s")
	require.Equal(t, t0.Add(15*time.Minute), s.ExpiresAt, "the end of the wait does not move")
	require.Empty(t, f.db.events, "a retry announces nothing")
	require.Equal(t, int64(10000), f.db.wallet.Balance().Minor())
}

func TestTheGapsGrowToTheCeilingAndNeverPastTheEndOfTheWait(t *testing.T) {
	f := newFixture(t, 10000)
	tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "bet-1"), 1)
	policyWide := policy
	policyWide.MaxAttempts = 100
	f.uc = resolvepending.New(resolvepending.Deps{UnitOfWork: f.db, Recorder: f.rec, Now: func() time.Time { return f.now }, NewID: uuid.New, Policy: policyWide})

	var gaps []time.Duration
	for range 8 {
		f.now = f.db.sched[tx.ID()].NextAttemptAt
		before := f.now
		f.try(t, tx)
		gaps = append(gaps, f.db.sched[tx.ID()].NextAttemptAt.Sub(before))
	}
	require.Equal(t, []time.Duration{4, 8, 16, 32, 60, 60, 60, 60}, divide(gaps), "2s, 4s, 8s... up to 60s")

	f.now = t0.Add(14*time.Minute + 50*time.Second)
	f.try(t, tx)
	require.Equal(t, t0.Add(15*time.Minute), f.db.sched[tx.ID()].NextAttemptAt, "the last try falls on the expiry")
}

func divide(gaps []time.Duration) []time.Duration {
	out := make([]time.Duration, len(gaps))
	for i, g := range gaps {
		out[i] = g / time.Second
	}
	return out
}

func TestWhenItMayNotWaitAnyMoreAMissingReferenceIsNotFound(t *testing.T) {
	cases := map[string]func(*fixture, wager.Transaction){
		"the attempts are used up": func(f *fixture, tx wager.Transaction) {
			f.db.sched[tx.ID()] = application.PendingSchedule{Attempts: policy.MaxAttempts - 1, NextAttemptAt: t0, ExpiresAt: t0.Add(15 * time.Minute)}
			f.now = t0.Add(time.Minute)
		},
		"the time is up": func(f *fixture, tx wager.Transaction) { f.now = t0.Add(15 * time.Minute) },
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 10000)
			tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "bet-1"), 1)
			arrange(f, tx)

			r := f.try(t, tx)

			require.Equal(t, resolvepending.OutcomeExpired, r.Outcome)
			done := f.db.txs[tx.ID()]
			require.Equal(t, wager.StatusRejected, done.Status())
			require.Equal(t, wager.FailureReferenceNotFound, done.FailureCode(), "the Reference never appeared")
			require.Equal(t, []string{"WagerTransactionRejected"}, types(f.db.events))
			require.Equal(t, int64(10000), f.db.wallet.Balance().Minor())
		})
	}
}

func TestAReferenceThatAppearedAndNeverCompletedIsNotProcessedWhenTheWaitEnds(t *testing.T) {
	f := newFixture(t, 10000)
	tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "win-1"), 1)
	// The Reference is itself waiting, for a bet that never comes.
	ref := f.waiting(t, params(wager.KindWin, "win-1", 100, "bet-never"), 1)
	f.now = t0.Add(15 * time.Minute)

	r := f.try(t, tx)

	require.Equal(t, resolvepending.OutcomeExpired, r.Outcome)
	require.Equal(t, wager.FailureReferenceNotProcessed, f.db.txs[tx.ID()].FailureCode())
	require.Equal(t, ref.ID(), f.db.txs[tx.ID()].External().ReferenceTransactionID, "the Reference it found is kept")
}

func TestAReferenceThatEndedWithoutSuccessRefusesAtOnce(t *testing.T) {
	f := newFixture(t, 10000)
	tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "bet-1"), 1)
	rejected, err := newTx(t, params(wager.KindBet, "bet-1", 2500, ""), t0).Reject(wager.FailureInsufficientFunds, t0)
	require.NoError(t, err)
	f.db.byExtID["bet-1"] = rejected
	f.db.txs[rejected.ID()] = rejected

	r := f.try(t, tx)

	require.Equal(t, resolvepending.OutcomeRejected, r.Outcome, "not an expiry: the wait did not run out")
	require.Equal(t, wager.FailureReferenceNotProcessed, f.db.txs[tx.ID()].FailureCode())
}

func TestARuleThatRefusesTheOperationRefusesItWhateverTheReference(t *testing.T) {
	f := newFixture(t, 10000)
	tx := f.waiting(t, params(wager.KindRefund, "refund-1", 999, "bet-1"), 1)
	f.processedBet(t, "bet-1", 2500)

	r := f.try(t, tx)

	require.Equal(t, resolvepending.OutcomeRejected, r.Outcome)
	require.Equal(t, wager.FailureReferenceAmountMismatch, f.db.txs[tx.ID()].FailureCode())
}

func TestAnOperationDecidedByAnotherWorkerIsLeftAlone(t *testing.T) {
	f := newFixture(t, 10000)
	tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "bet-1"), 1)
	f.processedBet(t, "bet-1", 2500)
	done, err := tx.Process(wager.Result{Balance: brl(12500), WalletVersion: 3}, t0.Add(time.Second))
	require.NoError(t, err)
	f.db.txs[tx.ID()] = done // decided between the reservation and the lock

	r := f.try(t, tx)

	require.Equal(t, resolvepending.OutcomeDiscarded, r.Outcome)
	require.Equal(t, []string{"lock", "read"}, f.db.calls)
	require.Empty(t, f.db.events)
	require.Equal(t, int64(10000), f.db.wallet.Balance().Minor(), "nothing moved twice")
}

func TestAnUnfinishedAcceptanceIsFinishedLikeANewOperation(t *testing.T) {
	f := newFixture(t, 10000)
	orphan := newTx(t, params(wager.KindBet, "bet-1", 2500, ""), t0) // PENDING, never decided
	f.db.txs[orphan.ID()] = orphan
	f.db.byExtID["bet-1"] = orphan

	r := f.try(t, orphan)

	require.Equal(t, resolvepending.OutcomeProcessed, r.Outcome)
	require.Equal(t, int64(7500), f.db.wallet.Balance().Minor())
	require.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, types(f.db.events))
}

func TestAnErrorLeavesTheOperationForALaterTry(t *testing.T) {
	for _, step := range []string{"lock", "read", "reference"} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t, 10000)
			tx := f.waiting(t, params(wager.KindRefund, "refund-1", 2500, "bet-1"), 1)
			f.processedBet(t, "bet-1", 2500)
			f.db.failAt = step

			_, err := f.uc.Execute(t.Context(), f.item(tx))

			require.Error(t, err)
			require.Empty(t, f.rec.outcomes, "a try that did not happen is not counted")
			require.Equal(t, wager.StatusPendingReference, f.db.txs[tx.ID()].Status())
		})
	}
}
