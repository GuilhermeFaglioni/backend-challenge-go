//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

func migrateDatabase(db infra.Database) error { return infra.Migrate(db.OwnerDSN) }

type countingRecorder struct {
	mu       sync.Mutex
	outcomes map[openwallet.Outcome]int
}

func (c *countingRecorder) Opened(o openwallet.Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.outcomes == nil {
		c.outcomes = map[openwallet.Outcome]int{}
	}
	c.outcomes[o]++
}

type openEnv struct {
	pool     *pgxpool.Pool
	recorder *countingRecorder
	useCase  *openwallet.UseCase
	now      time.Time
}

func newOpenEnv(t *testing.T, wrap func(application.UnitOfWork) application.UnitOfWork) openEnv {
	t.Helper()
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, migrateDatabase(db))
	pool := appPool(ctx, t, db.AppDSN)

	var uow application.UnitOfWork = postgres.NewUnitOfWork(pool)
	if wrap != nil {
		uow = wrap(uow)
	}
	rec := &countingRecorder{}
	now := time.Date(2026, 10, 14, 12, 0, 0, 123_000_000, time.UTC)
	return openEnv{
		pool: pool, recorder: rec, now: now,
		useCase: openwallet.New(openwallet.Deps{UnitOfWork: uow, Recorder: rec, Now: func() time.Time { return now }, NewID: uuid.New}),
	}
}

func brl(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	return m
}

func (e openEnv) tables(t *testing.T) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"wallets", "wager_transactions", "wallet_ledger_entries", "outbox"} {
		counts[table] = count(t.Context(), t, e.pool, table)
	}
	return counts
}

func TestOpeningAFundedWalletWritesEverythingAtomically(t *testing.T) {
	e := newOpenEnv(t, nil)
	ctx := t.Context()
	player := uuid.New()

	w, err := e.useCase.Execute(ctx, openwallet.Input{PlayerID: player, InitialBalance: brl(t, 5000), CorrelationID: "corr-1"})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"wallets": 1, "wager_transactions": 1, "wallet_ledger_entries": 1, "outbox": 2}, e.tables(t))

	var balance, version int64
	var lastTx uuid.UUID
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT balance_minor, version, last_transaction_id FROM wallets WHERE id = $1`, w.ID()).Scan(&balance, &version, &lastTx))
	require.Equal(t, int64(5000), balance)
	require.Equal(t, int64(1), version)

	var origin, kind, status string
	var resultBalance, resultVersion int64
	var correlation string
	var processedAt *time.Time
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT origin, kind, status, result_balance_minor, result_wallet_version, correlation_id, processed_at
		FROM wager_transactions WHERE id = $1 AND wallet_id = $2`, lastTx, w.ID()).Scan(&origin, &kind, &status, &resultBalance, &resultVersion, &correlation, &processedAt))
	require.Equal(t, []any{"INTERNAL", "OPENING", "PROCESSED", int64(5000), int64(1), "corr-1"}, []any{origin, kind, status, resultBalance, resultVersion, correlation})
	require.NotNil(t, processedAt)

	var direction string
	var amount, before, after int64
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT direction, amount_minor, balance_before_minor, balance_after_minor
		FROM wallet_ledger_entries WHERE wallet_id = $1 AND transaction_id = $2`, w.ID(), lastTx).Scan(&direction, &amount, &before, &after))
	require.Equal(t, []any{"CREDIT", int64(5000), int64(0), int64(5000)}, []any{direction, amount, before, after})

	t.Run("the outbox holds both events as immutable snapshots", func(t *testing.T) {
		rows, err := e.pool.Query(ctx, `SELECT event_type, version, aggregate_id, payload::text, correlation_id, causation_id, occurred_at, published_at FROM outbox ORDER BY event_type`)
		require.NoError(t, err)
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var eventType, payload, corr string
			var version int
			var aggregate uuid.UUID
			var causation *string
			var occurred time.Time
			var published *time.Time
			require.NoError(t, rows.Scan(&eventType, &version, &aggregate, &payload, &corr, &causation, &occurred, &published))
			require.Equal(t, 1, version)
			require.Equal(t, w.ID(), aggregate)
			require.Equal(t, "corr-1", corr)
			require.Nil(t, causation)
			require.True(t, e.now.Equal(occurred))
			require.Nil(t, published, "not published yet")
			got[eventType] = payload
		}
		require.NoError(t, rows.Err())

		require.JSONEq(t, `{"walletId":"`+w.ID().String()+`","transactionId":"`+lastTx.String()+`","direction":"CREDIT",
			"money":{"amount":"50.00","currency":"BRL"},"balanceBefore":{"amount":"0.00","currency":"BRL"},
			"balanceAfter":{"amount":"50.00","currency":"BRL"},"walletVersion":1}`, got["WalletBalanceChanged"])
		require.JSONEq(t, `{"transactionId":"`+lastTx.String()+`","walletId":"`+w.ID().String()+`","playerId":"`+player.String()+`",
			"kind":"OPENING","origin":"INTERNAL","money":{"amount":"50.00","currency":"BRL"},
			"balance":{"amount":"50.00","currency":"BRL"},"walletVersion":1}`, got["WagerTransactionProcessed"])
	})
	require.Equal(t, map[openwallet.Outcome]int{openwallet.OutcomeCreated: 1}, e.recorder.outcomes)
}

func TestOpeningAnEmptyWalletCreatesOnlyTheWallet(t *testing.T) {
	e := newOpenEnv(t, nil)
	w, err := e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: uuid.New(), InitialBalance: brl(t, 0), CorrelationID: "corr-1"})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"wallets": 1, "wager_transactions": 0, "wallet_ledger_entries": 0, "outbox": 0}, e.tables(t))

	var lastTx *uuid.UUID
	var balance int64
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT balance_minor, last_transaction_id FROM wallets WHERE id = $1`, w.ID()).Scan(&balance, &lastTx))
	require.Zero(t, balance)
	require.Nil(t, lastTx)
}

func TestASecondWalletForTheSamePlayerAndCurrencyIsATypedConflict(t *testing.T) {
	e := newOpenEnv(t, nil)
	player := uuid.New()
	first, err := e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: player, InitialBalance: brl(t, 5000), CorrelationID: "c"})
	require.NoError(t, err)
	before := e.tables(t)

	_, err = e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: player, InitialBalance: brl(t, 9000), CorrelationID: "c"})
	require.ErrorIs(t, err, application.ErrWalletAlreadyExists)
	var conflict *application.WalletAlreadyExistsError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, first.ID(), conflict.WalletID, "the answer names the wallet that is already there")
	require.Equal(t, before, e.tables(t), "the refused attempt wrote nothing, not even the opening it had prepared")
	require.Equal(t, 1, e.recorder.outcomes[openwallet.OutcomeConflict])

	other, err := money.FromMinor(100, money.USD)
	require.NoError(t, err)
	_, err = e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: player, InitialBalance: other, CorrelationID: "c"})
	require.NoError(t, err, "another currency is another wallet")
}

func TestManyConcurrentOpensProduceExactlyOneWallet(t *testing.T) {
	e := newOpenEnv(t, nil)
	player := uuid.New()
	const workers = 25

	var wg sync.WaitGroup
	results := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: player, InitialBalance: brl(t, 5000), CorrelationID: "c"})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	created, conflicts := 0, 0
	var ids = map[uuid.UUID]bool{}
	for err := range results {
		var conflict *application.WalletAlreadyExistsError
		switch {
		case err == nil:
			created++
		case errors.As(err, &conflict):
			conflicts++
			ids[conflict.WalletID] = true
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, workers-1, conflicts)
	require.Len(t, ids, 1, "every loser is told about the same wallet")
	require.Equal(t, map[string]int{"wallets": 1, "wager_transactions": 1, "wallet_ledger_entries": 1, "outbox": 2}, e.tables(t))
}

// --- a failure at any step leaves nothing behind ----------------------------------

type failingUnitOfWork struct {
	inner  application.UnitOfWork
	failAt string
	err    error
}

func (f failingUnitOfWork) Do(ctx context.Context, fn func(context.Context, application.Repositories) error) error {
	return f.inner.Do(ctx, func(ctx context.Context, repos application.Repositories) error {
		return fn(ctx, failingRepositories{Repositories: repos, failAt: f.failAt, err: f.err, outboxCalls: new(int)})
	})
}

type failingRepositories struct {
	application.Repositories
	failAt      string
	err         error
	outboxCalls *int
}

func (f failingRepositories) Wallets() application.WalletRepository {
	return failingWallets{f.Repositories.Wallets(), f.failAt == "wallet", f.err}
}
func (f failingRepositories) Transactions() application.TransactionRepository {
	return failingTransactions{f.Repositories.Transactions(), f.failAt == "transaction", f.err}
}
func (f failingRepositories) Ledger() application.LedgerRepository {
	return failingLedger{f.Repositories.Ledger(), f.failAt == "ledger", f.err}
}
func (f failingRepositories) Outbox() application.OutboxRepository {
	return failingOutbox{f.Repositories.Outbox(), f.failAt, f.err, f.outboxCalls}
}

// Each wrapper performs the real write and then fails: the worst case, where the
// step's own row is already in the transaction.
type failingWallets struct {
	application.WalletRepository
	fail bool
	err  error
}

func (f failingWallets) Insert(ctx context.Context, w wallet.Wallet) error {
	if err := f.WalletRepository.Insert(ctx, w); err != nil {
		return err
	}
	if f.fail {
		return f.err
	}
	return nil
}

type failingTransactions struct {
	application.TransactionRepository
	fail bool
	err  error
}

func (f failingTransactions) InsertOpening(ctx context.Context, tx wager.Transaction) error {
	if err := f.TransactionRepository.InsertOpening(ctx, tx); err != nil {
		return err
	}
	if f.fail {
		return f.err
	}
	return nil
}

type failingLedger struct {
	application.LedgerRepository
	fail bool
	err  error
}

func (f failingLedger) Append(ctx context.Context, e wallet.LedgerEntry) error {
	if err := f.LedgerRepository.Append(ctx, e); err != nil {
		return err
	}
	if f.fail {
		return f.err
	}
	return nil
}

type failingOutbox struct {
	application.OutboxRepository
	failAt string
	err    error
	calls  *int
}

func (f failingOutbox) Add(ctx context.Context, e event.Event) error {
	if err := f.OutboxRepository.Add(ctx, e); err != nil {
		return err
	}
	*f.calls++
	if (f.failAt == "first event" && *f.calls == 1) || (f.failAt == "second event" && *f.calls == 2) {
		return f.err
	}
	return nil
}

func TestAFailureAtAnyStepLeavesNothingBehind(t *testing.T) {
	boom := errors.New("injected failure")
	for _, step := range []string{"wallet", "transaction", "ledger", "first event", "second event"} {
		t.Run(step, func(t *testing.T) {
			e := newOpenEnv(t, func(inner application.UnitOfWork) application.UnitOfWork {
				return failingUnitOfWork{inner: inner, failAt: step, err: boom}
			})
			_, err := e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: uuid.New(), InitialBalance: brl(t, 5000), CorrelationID: "c"})
			require.ErrorIs(t, err, boom)
			require.Equal(t, map[string]int{"wallets": 0, "wager_transactions": 0, "wallet_ledger_entries": 0, "outbox": 0}, e.tables(t),
				"everything written before the failure is rolled back")
			require.Equal(t, 1, e.recorder.outcomes[openwallet.OutcomeError])
		})
	}
}

// A database refusal at commit (the coherence triggers fire there) rolls back too:
// here the wallet claims a balance, but the ledger entry is withheld.
func TestAWalletWithoutItsEntryIsRefusedAtCommit(t *testing.T) {
	e := newOpenEnv(t, func(inner application.UnitOfWork) application.UnitOfWork {
		return skippingLedgerUnitOfWork{inner}
	})
	_, err := e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: uuid.New(), InitialBalance: brl(t, 5000), CorrelationID: "c"})
	require.Error(t, err)
	require.Equal(t, checkViolation, sqlState(err))
	require.Equal(t, map[string]int{"wallets": 0, "wager_transactions": 0, "wallet_ledger_entries": 0, "outbox": 0}, e.tables(t))
}

type skippingLedgerUnitOfWork struct{ inner application.UnitOfWork }

func (s skippingLedgerUnitOfWork) Do(ctx context.Context, fn func(context.Context, application.Repositories) error) error {
	return s.inner.Do(ctx, func(ctx context.Context, repos application.Repositories) error {
		return fn(ctx, skippingLedger{repos})
	})
}

type skippingLedger struct{ application.Repositories }

func (s skippingLedger) Ledger() application.LedgerRepository { return noLedger{} }

type noLedger struct{}

func (noLedger) Append(context.Context, wallet.LedgerEntry) error { return nil }

func TestInvalidInputOpensNothing(t *testing.T) {
	e := newOpenEnv(t, nil)
	_, err := e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: uuid.Nil, InitialBalance: brl(t, 100), CorrelationID: "c"})
	require.ErrorIs(t, err, openwallet.ErrInvalidInput)
	require.Equal(t, map[string]int{"wallets": 0, "wager_transactions": 0, "wallet_ledger_entries": 0, "outbox": 0}, e.tables(t))
	require.Equal(t, 1, e.recorder.outcomes[openwallet.OutcomeInvalid])
}

// What the outbox stores is the event's own snapshot: reading it back and
// serializing the event again gives the same bytes.
func TestTheStoredPayloadIsTheEventsOwnSnapshot(t *testing.T) {
	e := newOpenEnv(t, nil)
	_, err := e.useCase.Execute(t.Context(), openwallet.Input{PlayerID: uuid.New(), InitialBalance: brl(t, 123), CorrelationID: "c"})
	require.NoError(t, err)
	var payload string
	require.NoError(t, e.pool.QueryRow(t.Context(), `SELECT payload::text FROM outbox WHERE event_type = 'WalletBalanceChanged'`).Scan(&payload))
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &parsed))
	require.Equal(t, "1.23", parsed["money"].(map[string]any)["amount"], "money is a decimal string, never a number")
}
