//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

type wagerRecorder struct {
	mu       sync.Mutex
	outcomes map[processwager.Outcome]int
}

func (r *wagerRecorder) Handled(_ wager.Kind, o processwager.Outcome, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcomes == nil {
		r.outcomes = map[processwager.Outcome]int{}
	}
	r.outcomes[o]++
}

func (r *wagerRecorder) count(o processwager.Outcome) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcomes[o]
}

type retryCounter struct{ deadlocks, serialization, concurrent atomic.Int64 }

func (c *retryCounter) Retried(reason application.RetryReason) {
	switch reason {
	case application.RetryDeadlock:
		c.deadlocks.Add(1)
	case application.RetrySerializationFailure:
		c.serialization.Add(1)
	case application.RetryConcurrentUpdate:
		c.concurrent.Add(1)
	}
}

type wagerEnv struct {
	pool    *pgxpool.Pool
	owner   *schema
	uow     *postgres.UnitOfWork
	opener  *openwallet.UseCase
	uc      *processwager.UseCase
	rec     *wagerRecorder
	retries *retryCounter
}

func newWagerEnv(t *testing.T) *wagerEnv {
	t.Helper()
	s := newSchema(t)
	pool := appPool(s.ctx, t, s.appDSN)
	retries := &retryCounter{}
	uow := postgres.NewUnitOfWork(pool).WithObserver(retries)
	rec := &wagerRecorder{}
	return &wagerEnv{
		pool: pool, owner: s, uow: uow, rec: rec, retries: retries,
		opener: openwallet.New(openwallet.Deps{UnitOfWork: uow, Recorder: &countingRecorder{}, Now: time.Now, NewID: uuid.New}),
		uc:     processwager.New(processwager.Deps{UnitOfWork: uow, Recorder: rec, Now: time.Now, NewID: uuid.New}),
	}
}

type wagerWallet struct{ id, player uuid.UUID }

// openWallet opens a funded wallet through the real use case.
func (e *wagerEnv) openWallet(t *testing.T, minor int64) wagerWallet {
	t.Helper()
	player := uuid.New()
	w, err := e.opener.Execute(t.Context(), openwallet.Input{PlayerID: player, InitialBalance: brl(t, minor), CorrelationID: "setup"})
	require.NoError(t, err)
	return wagerWallet{id: w.ID(), player: player}
}

func (e *wagerEnv) bet(w wagerWallet, provider, externalID, key string, minor int64) processwager.Input {
	amount, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		panic(err)
	}
	return processwager.Input{
		Request: wager.RequestParams{
			ProviderID: provider, ExternalTransactionID: externalID, PlayerID: w.player, WalletID: w.id,
			RoundID: "round-1", GameID: "game-1", Kind: wager.KindBet, Amount: amount,
		},
		IdempotencyKey: key, CorrelationID: "corr-" + externalID, CausationID: "cause-" + externalID,
	}
}

func (e *wagerEnv) balance(t *testing.T, id uuid.UUID) (balance, version int64) {
	t.Helper()
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT balance_minor, version FROM wallets WHERE id = $1`, id).Scan(&balance, &version))
	return balance, version
}

func (e *wagerEnv) countWhere(t *testing.T, table, where string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE `+where, args...).Scan(&n))
	return n
}

// --- one bet, atomically --------------------------------------------------------------

func TestABetIsProcessedAndEverythingIsStoredTogether(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)

	out, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
	require.NoError(t, err)
	require.False(t, out.IdempotentReplay)
	require.Equal(t, wager.StatusProcessed, out.Transaction.Status())

	balance, version := e.balance(t, w.id)
	require.Equal(t, int64(7500), balance)
	require.Equal(t, int64(2), version)

	var status, hash, key, correlation string
	var resultBalance, resultVersion int64
	var walletID, requestedID uuid.UUID
	var processedAt *time.Time
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT status, payload_hash, idempotency_key, correlation_id, result_balance_minor, result_wallet_version, wallet_id, requested_wallet_id, processed_at
		FROM wager_transactions WHERE id = $1`, out.Transaction.ID()).Scan(&status, &hash, &key, &correlation, &resultBalance, &resultVersion, &walletID, &requestedID, &processedAt))
	require.Equal(t, []any{"PROCESSED", string(out.Transaction.External().PayloadHash), "key-1", "corr-tx-1", int64(7500), int64(2), w.id, w.id},
		[]any{status, hash, key, correlation, resultBalance, resultVersion, walletID, requestedID})
	require.NotNil(t, processedAt)

	var direction string
	var amount, before, after int64
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT direction, amount_minor, balance_before_minor, balance_after_minor FROM wallet_ledger_entries WHERE transaction_id = $1`,
		out.Transaction.ID()).Scan(&direction, &amount, &before, &after))
	require.Equal(t, []any{"DEBIT", int64(2500), int64(10000), int64(7500)}, []any{direction, amount, before, after})

	require.Equal(t, 2, e.countWhere(t, "outbox", "event_type IN ('WagerTransactionProcessed', 'WalletBalanceChanged') AND payload->>'transactionId' = $1", out.Transaction.ID().String()))
	var causation *string
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT causation_id FROM outbox WHERE payload->>'transactionId' = $1 AND event_type = 'WalletBalanceChanged'`, out.Transaction.ID().String()).Scan(&causation))
	require.NotNil(t, causation)
	require.Equal(t, "cause-tx-1", *causation)
}

func TestAReplayAnswersFromStorageWithTheBalanceOfTheFirstTimeEvenAfterOtherMovements(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	first, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
	require.NoError(t, err)
	_, err = e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-2", "key-2", 4000))
	require.NoError(t, err)
	balance, _ := e.balance(t, w.id)
	require.Equal(t, int64(3500), balance)

	rowsBefore := map[string]int{"tx": e.countWhere(t, "wager_transactions", "true"), "ledger": e.countWhere(t, "wallet_ledger_entries", "true"), "outbox": e.countWhere(t, "outbox", "true")}
	replay, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
	require.NoError(t, err)

	require.True(t, replay.IdempotentReplay)
	require.Equal(t, first.Transaction.ID(), replay.Transaction.ID())
	result, _ := replay.Transaction.Result()
	require.Equal(t, int64(7500), result.Balance.Minor(), "the balance at the original processing, not today's 35.00")
	require.Equal(t, rowsBefore, map[string]int{"tx": e.countWhere(t, "wager_transactions", "true"), "ledger": e.countWhere(t, "wallet_ledger_entries", "true"), "outbox": e.countWhere(t, "outbox", "true")},
		"a replay writes nothing")
	require.Equal(t, 1, e.rec.count(processwager.OutcomeReplayed))
}

func TestConflictsApplyNothingAndProvidersAreIsolated(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	_, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
	require.NoError(t, err)
	ledger := e.countWhere(t, "wallet_ledger_entries", "true")

	_, err = e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2501))
	require.ErrorIs(t, err, processwager.ErrIdempotencyKeyConflict, "same key, other content")
	_, err = e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-other", 2500))
	require.ErrorIs(t, err, processwager.ErrDuplicateExternalTransaction, "same external id, other key")
	balance, _ := e.balance(t, w.id)
	require.Equal(t, int64(7500), balance)
	require.Equal(t, ledger, e.countWhere(t, "wallet_ledger_entries", "true"))

	out, err := e.uc.Execute(t.Context(), e.bet(w, "provider-b", "tx-1", "key-1", 2500))
	require.NoError(t, err, "the same key and external id belong to provider B alone")
	require.False(t, out.IdempotentReplay)
	balance, _ = e.balance(t, w.id)
	require.Equal(t, int64(5000), balance)
}

// --- rejections --------------------------------------------------------------------

func TestRejectionsAreStoredFinalAndReplayable(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 5000)

	t.Run("insufficient funds", func(t *testing.T) {
		out, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-big", "key-big", 8000))
		require.NoError(t, err)
		require.Equal(t, wager.StatusRejected, out.Transaction.Status())
		require.Equal(t, wager.FailureInsufficientFunds, out.Transaction.FailureCode())

		balance, version := e.balance(t, w.id)
		require.Equal(t, int64(5000), balance)
		require.Equal(t, int64(1), version, "a rejection leaves the version alone")
		require.Zero(t, e.countWhere(t, "wallet_ledger_entries", "transaction_id = $1", out.Transaction.ID()))
		require.Equal(t, 1, e.countWhere(t, "outbox", "event_type = 'WagerTransactionRejected' AND payload->>'transactionId' = $1", out.Transaction.ID().String()))

		// Terminal: topping the wallet up does not turn the same request into a success.
		replay, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-big", "key-big", 8000))
		require.NoError(t, err)
		require.True(t, replay.IdempotentReplay)
		require.Equal(t, wager.FailureInsufficientFunds, replay.Transaction.FailureCode())
	})

	t.Run("wallet not found, stored with the requested id", func(t *testing.T) {
		missing := wagerWallet{id: uuid.New(), player: w.player}
		out, err := e.uc.Execute(t.Context(), e.bet(missing, "provider-a", "tx-nowallet", "key-nowallet", 1000))
		require.NoError(t, err)
		require.Equal(t, wager.FailureWalletNotFound, out.Transaction.FailureCode())

		var storedWallet *uuid.UUID
		var requested uuid.UUID
		require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT wallet_id, requested_wallet_id FROM wager_transactions WHERE id = $1`, out.Transaction.ID()).Scan(&storedWallet, &requested))
		require.Nil(t, storedWallet, "there is no wallet to point at")
		require.Equal(t, missing.id, requested)
		require.Equal(t, 1, e.countWhere(t, "outbox", "event_type = 'WagerTransactionRejected' AND aggregate_id = $1", missing.id))

		replay, err := e.uc.Execute(t.Context(), e.bet(missing, "provider-a", "tx-nowallet", "key-nowallet", 1000))
		require.NoError(t, err)
		require.True(t, replay.IdempotentReplay)
		require.Equal(t, wager.FailureWalletNotFound, replay.Transaction.FailureCode())
		require.Equal(t, missing.id, replay.Transaction.RequestedWalletID())
	})

	t.Run("player mismatch", func(t *testing.T) {
		stranger := wagerWallet{id: w.id, player: uuid.New()}
		out, err := e.uc.Execute(t.Context(), e.bet(stranger, "provider-a", "tx-player", "key-player", 100))
		require.NoError(t, err)
		require.Equal(t, wager.FailurePlayerMismatch, out.Transaction.FailureCode())
	})

	t.Run("currency mismatch", func(t *testing.T) {
		in := e.bet(w, "provider-a", "tx-currency", "key-currency", 100)
		usd, err := money.FromMinor(100, money.USD)
		require.NoError(t, err)
		in.Request.Amount = usd
		out, err := e.uc.Execute(t.Context(), in)
		require.NoError(t, err)
		require.Equal(t, wager.FailureCurrencyMismatch, out.Transaction.FailureCode())
	})

	balance, version := e.balance(t, w.id)
	require.Equal(t, int64(5000), balance, "none of the rejections moved money")
	require.Equal(t, int64(1), version)
}

// --- atomicity ---------------------------------------------------------------------

type stepFailure struct {
	inner application.UnitOfWork
	step  string
}

func (f stepFailure) Do(ctx context.Context, fn func(context.Context, application.Repositories) error) error {
	return f.inner.Do(ctx, func(ctx context.Context, repos application.Repositories) error {
		return fn(ctx, failingStepRepos{Repositories: repos, step: f.step})
	})
}

var errStep = errors.New("injected failure")

type failingStepRepos struct {
	application.Repositories
	step string
}

func (r failingStepRepos) Wallets() application.WalletRepository {
	return failingStepWallets{r.Repositories.Wallets(), r.step == "update"}
}
func (r failingStepRepos) Transactions() application.TransactionRepository {
	return failingStepTransactions{r.Repositories.Transactions(), r.step}
}
func (r failingStepRepos) Ledger() application.LedgerRepository {
	return failingStepLedger{r.Repositories.Ledger(), r.step == "ledger"}
}
func (r failingStepRepos) Outbox() application.OutboxRepository {
	return failingStepOutbox{r.Repositories.Outbox(), r.step == "outbox"}
}

// Each wrapper does the real write and then fails: the worst case, with the step's own
// row already inside the database transaction.
type failingStepWallets struct {
	application.WalletRepository
	fail bool
}

func (w failingStepWallets) Update(ctx context.Context, before, after wallet.Wallet) error {
	if err := w.WalletRepository.Update(ctx, before, after); err != nil {
		return err
	}
	if w.fail {
		return errStep
	}
	return nil
}

type failingStepTransactions struct {
	application.TransactionRepository
	step string
}

func (t failingStepTransactions) Claim(ctx context.Context, tx wager.Transaction) (bool, error) {
	claimed, err := t.TransactionRepository.Claim(ctx, tx)
	if err == nil && t.step == "claim" {
		return false, errStep
	}
	return claimed, err
}

func (t failingStepTransactions) Finish(ctx context.Context, tx wager.Transaction) error {
	if err := t.TransactionRepository.Finish(ctx, tx); err != nil {
		return err
	}
	if t.step == "finish" {
		return errStep
	}
	return nil
}

type failingStepLedger struct {
	application.LedgerRepository
	fail bool
}

func (l failingStepLedger) Append(ctx context.Context, e wallet.LedgerEntry) error {
	if err := l.LedgerRepository.Append(ctx, e); err != nil {
		return err
	}
	if l.fail {
		return errStep
	}
	return nil
}

type failingStepOutbox struct {
	application.OutboxRepository
	fail bool
}

func (o failingStepOutbox) Add(ctx context.Context, e event.Event) error {
	if err := o.OutboxRepository.Add(ctx, e); err != nil {
		return err
	}
	if o.fail {
		return errStep
	}
	return nil
}

func TestAFailureAfterAnyWriteLeavesNothingBehind(t *testing.T) {
	for _, step := range []string{"claim", "update", "ledger", "finish", "outbox"} {
		t.Run(step, func(t *testing.T) {
			e := newWagerEnv(t)
			w := e.openWallet(t, 10000)
			before := map[string]int{"tx": e.countWhere(t, "wager_transactions", "true"), "ledger": e.countWhere(t, "wallet_ledger_entries", "true"), "outbox": e.countWhere(t, "outbox", "true")}

			failing := processwager.New(processwager.Deps{UnitOfWork: stepFailure{inner: e.uow, step: step}, Recorder: e.rec, Now: time.Now, NewID: uuid.New})
			_, err := failing.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
			require.ErrorIs(t, err, errStep)

			balance, version := e.balance(t, w.id)
			require.Equal(t, int64(10000), balance)
			require.Equal(t, int64(1), version)
			require.Equal(t, before, map[string]int{"tx": e.countWhere(t, "wager_transactions", "true"), "ledger": e.countWhere(t, "wallet_ledger_entries", "true"), "outbox": e.countWhere(t, "outbox", "true")},
				"not even the claim of the key survives")

			out, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
			require.NoError(t, err, "and the request, sent again, is a new one")
			require.False(t, out.IdempotentReplay)
		})
	}
}

func TestATransientErrorSurfacesAsSuchAndStoresNothing(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := e.uc.Execute(ctx, e.bet(w, "provider-a", "tx-1", "key-1", 2500))
	require.ErrorIs(t, err, application.ErrTemporarilyUnavailable)
	require.Equal(t, 1, e.rec.count(processwager.OutcomeUnavailable))
	require.Zero(t, e.countWhere(t, "wager_transactions", "provider_id = 'provider-a'"))
}

// --- concurrency -------------------------------------------------------------------

// The challenge's scenario: 100.00 and two bets of 80.00 at the same time. Exactly one
// goes through. Repeated on many wallets at once.
func TestTwoBetsThatDoNotBothFitOnlyOneWins(t *testing.T) {
	e := newWagerEnv(t)
	const wallets = 20
	var wg sync.WaitGroup
	for i := 0; i < wallets; i++ {
		w := e.openWallet(t, 10000)
		for n := 0; n < 2; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in := e.bet(w, "provider-a", fmt.Sprintf("tx-%s-%d", w.id, n), fmt.Sprintf("key-%s-%d", w.id, n), 8000)
				_, err := e.uc.Execute(t.Context(), in)
				if err != nil {
					t.Errorf("wallet %s: %v", w.id, err)
				}
			}()
		}
	}
	wg.Wait()

	rows, err := e.pool.Query(t.Context(), `SELECT w.id, w.balance_minor,
		(SELECT count(*) FROM wager_transactions t WHERE t.wallet_id = w.id AND t.kind = 'BET' AND t.status = 'PROCESSED'),
		(SELECT count(*) FROM wager_transactions t WHERE t.wallet_id = w.id AND t.kind = 'BET' AND t.status = 'REJECTED' AND t.failure_code = 'INSUFFICIENT_FUNDS'),
		(SELECT count(*) FROM wallet_ledger_entries l WHERE l.wallet_id = w.id AND l.direction = 'DEBIT')
		FROM wallets w`)
	require.NoError(t, err)
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id uuid.UUID
		var balance, processed, rejected, debits int64
		require.NoError(t, rows.Scan(&id, &balance, &processed, &rejected, &debits))
		require.Equal(t, []int64{2000, 1, 1, 1}, []int64{balance, processed, rejected, debits}, "wallet %s", id)
		seen++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, wallets, seen)
	require.Zero(t, e.retries.deadlocks.Load(), "operations on one wallet must not deadlock")
	// The lock, not the retry of a lost race, is what keeps the bets apart: with the lock
	// there is no race to lose. (The version-conditioned update is the second line of
	// defence, and would also give the right balances; it would show up here.)
	require.Zero(t, e.retries.concurrent.Load(), "the wallet lock must keep operations from racing")
	require.Zero(t, e.retries.serialization.Load())
}

func TestManySimultaneousCopiesOfOneRequestApplyItOnce(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	const copies = 50

	var wg sync.WaitGroup
	outputs := make(chan processwager.Output, copies)
	start := make(chan struct{})
	for i := 0; i < copies; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
			if err != nil {
				t.Errorf("%v", err)
				return
			}
			outputs <- out
		}()
	}
	close(start)
	wg.Wait()
	close(outputs)

	replays, originals := 0, 0
	ids := map[uuid.UUID]bool{}
	for out := range outputs {
		ids[out.Transaction.ID()] = true
		result, _ := out.Transaction.Result()
		require.Equal(t, int64(7500), result.Balance.Minor(), "every copy is told the same balance")
		if out.IdempotentReplay {
			replays++
		} else {
			originals++
		}
	}
	require.Equal(t, 1, originals)
	require.Equal(t, copies-1, replays)
	require.Len(t, ids, 1, "all of them are the same transaction")
	balance, _ := e.balance(t, w.id)
	require.Equal(t, int64(7500), balance, "debited once")
	require.Equal(t, 1, e.countWhere(t, "wallet_ledger_entries", "direction = 'DEBIT'"))
}

// A mixture of everything at once on three wallets, from many goroutines: new bets,
// replays, duplicates, conflicts and unknown wallets, in whatever order the scheduler
// likes. Nothing may deadlock, and in the end the money adds up, to the cent.
func TestInterleavedOperationsOnSharedWalletsNeverDeadlockAndTheMoneyAddsUp(t *testing.T) {
	e := newWagerEnv(t)
	const initial = 100_000
	wallets := []wagerWallet{e.openWallet(t, initial), e.openWallet(t, initial), e.openWallet(t, initial)}

	const workers, perWorker = 24, 25
	var processed, rejected, replays, conflicts, duplicates atomic.Int64
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(worker), 99)) //nolint:gosec // reproducible, not security related
			for i := 0; i < perWorker; i++ {
				// Few distinct ids, so that workers collide on keys and external ids; and
				// the wallet and amount follow from the id, so that a repeat is the same
				// request (a replay) unless the test changes it on purpose below.
				n := rng.IntN(60)
				w := wallets[n%len(wallets)]
				in := e.bet(w, "provider-a", fmt.Sprintf("tx-%d", n), fmt.Sprintf("key-%d", n), int64(100+(n*137)%6000))
				switch rng.IntN(8) {
				case 0: // a different payload under the same key
					in.Request.Amount = mustMoney(7)
				case 1: // the same external id under another key
					in.IdempotencyKey = fmt.Sprintf("other-key-%d-%d", n, rng.IntN(1000))
				case 2: // a wallet that does not exist
					in.Request.WalletID = uuid.New()
				}
				out, err := e.uc.Execute(t.Context(), in)
				switch {
				case err == nil && out.IdempotentReplay:
					replays.Add(1)
				case err == nil && out.Transaction.Status() == wager.StatusProcessed:
					processed.Add(1)
				case err == nil:
					rejected.Add(1)
				case errors.Is(err, processwager.ErrIdempotencyKeyConflict):
					conflicts.Add(1)
				case errors.Is(err, processwager.ErrDuplicateExternalTransaction):
					duplicates.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	require.Zero(t, e.retries.deadlocks.Load(), "the lock order (stored answer, wallet, claim) cannot deadlock")
	require.Zero(t, e.retries.concurrent.Load(), "the wallet lock must keep operations from racing")
	t.Logf("processed=%d rejected=%d replays=%d conflicts=%d duplicates=%d retries(serialization=%d concurrent=%d)",
		processed.Load(), rejected.Load(), replays.Load(), conflicts.Load(), duplicates.Load(), e.retries.serialization.Load(), e.retries.concurrent.Load())
	require.Positive(t, processed.Load())

	// Reconciliation: every wallet's balance is what its ledger says, and the ledger chains.
	for _, w := range wallets {
		var balance, version, debits, entries int64
		require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT balance_minor, version FROM wallets WHERE id = $1`, w.id).Scan(&balance, &version))
		require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT coalesce(sum(amount_minor), 0), count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id).Scan(&debits, &entries))
		require.Equal(t, int64(initial)-debits, balance, "wallet %s: initial minus its debits", w.id)
		require.GreaterOrEqual(t, balance, int64(0))
		require.Equal(t, 1+entries, version, "one version step per debit")

		// Only debits happen here, so the balance only goes down: ordered from the highest
		// balance, each entry must start where the previous one ended.
		var broken int
		require.NoError(t, e.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM (
				SELECT balance_before_minor, lag(balance_after_minor) OVER (ORDER BY balance_before_minor DESC) AS previous_after
				FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT') c
			WHERE previous_after IS NOT NULL AND previous_after <> balance_before_minor`, w.id).Scan(&broken))
		require.Zero(t, broken, "wallet %s: the ledger chains", w.id)
		if entries > 0 {
			var first int64
			require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT max(balance_before_minor) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id).Scan(&first))
			require.Equal(t, int64(initial), first, "and starts from the opening balance")
		}
	}
	var wrong int
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT count(*) FROM wager_transactions WHERE status = 'PENDING'`).Scan(&wrong))
	require.Zero(t, wrong, "no claimed transaction was left unfinished")
}

func mustMoney(minor int64) money.Money {
	m, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		panic(err)
	}
	return m
}

// --- the order of the locks ---------------------------------------------------------

// A replay is answered before any lock: a wallet that someone else is holding does not
// make it wait. A new operation on that wallet does wait, and goes on when it is released.
func TestAReplayDoesNotWaitForTheWalletLockButANewBetDoes(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	_, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-1", "key-1", 2500))
	require.NoError(t, err)

	holder, err := e.owner.owner.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	_, err = holder.Exec(t.Context(), `SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE`, w.id)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	replay, err := e.uc.Execute(ctx, e.bet(w, "provider-a", "tx-1", "key-1", 2500))
	require.NoError(t, err, "the replay waited for a lock it does not need")
	require.True(t, replay.IdempotentReplay)
	require.Less(t, time.Since(start), 2*time.Second)

	finished := make(chan error, 1)
	go func() {
		_, err := e.uc.Execute(t.Context(), e.bet(w, "provider-a", "tx-2", "key-2", 1000))
		finished <- err
	}()
	select {
	case err := <-finished:
		t.Fatalf("a new bet went through while the wallet was locked (err=%v)", err)
	case <-time.After(400 * time.Millisecond):
	}
	require.NoError(t, holder.Commit(t.Context()))
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the bet did not go on after the lock was released")
	}
	balance, _ := e.balance(t, w.id)
	require.Equal(t, int64(6500), balance)
}

// The wallet lock is FOR NO KEY UPDATE, not FOR UPDATE (ARCHITECTURE.md, section 7): it
// serializes writers but does not conflict with the FOR KEY SHARE that an insert into
// wager_transactions or the ledger takes on the wallet to check its foreign key.
func TestTheWalletLockDoesNotBlockTheForeignKeyChecksOfInserts(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)

	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- e.uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
			if _, err := repos.Wallets().Lock(ctx, w.id); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := e.owner.owner.Exec(ctx, `INSERT INTO wager_transactions
		(id, origin, kind, provider_id, external_transaction_id, idempotency_key, payload_hash, requested_wallet_id, wallet_id, player_id,
		 amount_minor, currency, status, failure_code, created_at, updated_at, processed_at)
		VALUES (gen_random_uuid(), 'EXTERNAL', 'BET', 'p', 'e', 'k', 'h', $1, $1, $2, 1, 'BRL', 'REJECTED', 'INSUFFICIENT_FUNDS', now(), now(), now())`, w.id, w.player)
	require.NoError(t, err, "the insert waited for the wallet lock to end: the lock is too strong")

	close(release)
	require.NoError(t, <-done)
}

func TestTheOutcomesOfARealRunAreWhatHappened(t *testing.T) {
	e := newWagerEnv(t)
	w := e.openWallet(t, 10000)
	for _, in := range []processwager.Input{
		e.bet(w, "provider-a", "tx-1", "key-1", 2500),
		e.bet(w, "provider-a", "tx-1", "key-1", 2500),
		e.bet(w, "provider-a", "tx-2", "key-2", 999999),
	} {
		_, err := e.uc.Execute(t.Context(), in)
		require.NoError(t, err)
	}
	require.Equal(t, 1, e.rec.count(processwager.OutcomeProcessed))
	require.Equal(t, 1, e.rec.count(processwager.OutcomeReplayed))
	require.Equal(t, 1, e.rec.count(processwager.OutcomeRejected))
}
