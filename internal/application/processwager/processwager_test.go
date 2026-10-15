package processwager_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

var (
	now    = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	player = uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7")
	wid    = uuid.MustParse("3fa85f64-5717-4562-b3fc-2c963f66afa6")
)

func brl(minor int64) money.Money {
	m, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		panic(err)
	}
	return m
}

// --- an in-memory database with the semantics the use case relies on ----------------

type key struct{ provider, value string }

// memDB reproduces what the use case needs from PostgreSQL: unique idempotency keys and
// external ids per provider, a version-conditioned wallet update, and rollback of
// everything a failed unit of work wrote. It records the order of the calls.
type memDB struct {
	wallets map[uuid.UUID]wallet.Wallet
	byKey   map[key]wager.Transaction
	byExt   map[key]wager.Transaction
	byID    map[uuid.UUID]wager.Transaction
	entries []wallet.LedgerEntry
	events  []event.Event

	calls       []string
	failAt      string // the call that fails with errInjected
	beforeClaim func(db *memDB)

	uowRuns int
	saved   snapshot // the committed state the running unit of work would roll back to
}

var errInjected = errors.New("injected failure")

func newDB() *memDB {
	return &memDB{
		wallets: map[uuid.UUID]wallet.Wallet{}, byKey: map[key]wager.Transaction{}, byExt: map[key]wager.Transaction{}, byID: map[uuid.UUID]wager.Transaction{},
	}
}

func (db *memDB) addWallet(w wallet.Wallet) { db.wallets[w.ID()] = w }

func (db *memDB) step(name string) error {
	db.calls = append(db.calls, name)
	if db.failAt == name {
		return errInjected
	}
	return nil
}

type snapshot struct {
	wallets map[uuid.UUID]wallet.Wallet
	byKey   map[key]wager.Transaction
	byExt   map[key]wager.Transaction
	byID    map[uuid.UUID]wager.Transaction
	entries []wallet.LedgerEntry
	events  []event.Event
}

func (db *memDB) save() snapshot {
	return snapshot{maps.Clone(db.wallets), maps.Clone(db.byKey), maps.Clone(db.byExt), maps.Clone(db.byID), slices.Clone(db.entries), slices.Clone(db.events)}
}

func (db *memDB) restore(s snapshot) {
	db.wallets, db.byKey, db.byExt, db.byID, db.entries, db.events = s.wallets, s.byKey, s.byExt, s.byID, s.entries, s.events
}

func (db *memDB) Do(ctx context.Context, fn func(context.Context, application.Repositories) error) error {
	db.uowRuns++
	db.saved = db.save()
	if err := fn(ctx, db); err != nil {
		db.restore(db.saved) // rollback
		return err
	}
	return nil
}

func (db *memDB) Wallets() application.WalletRepository           { return (*walletRepo)(db) }
func (db *memDB) Transactions() application.TransactionRepository { return (*txRepo)(db) }
func (db *memDB) Ledger() application.LedgerRepository            { return (*ledgerRepo)(db) }
func (db *memDB) Outbox() application.OutboxRepository            { return (*outboxRepo)(db) }

type (
	walletRepo (memDB)
	txRepo     (memDB)
	ledgerRepo (memDB)
	outboxRepo (memDB)
)

func (r *walletRepo) Insert(context.Context, wallet.Wallet) error { panic("not used") }

func (r *walletRepo) Lock(_ context.Context, id uuid.UUID) (wallet.Wallet, error) {
	db := (*memDB)(r)
	if err := db.step("lock"); err != nil {
		return wallet.Wallet{}, err
	}
	w, ok := db.wallets[id]
	if !ok {
		return wallet.Wallet{}, application.ErrWalletNotFound
	}
	return w, nil
}

func (r *walletRepo) Update(_ context.Context, before, after wallet.Wallet) error {
	db := (*memDB)(r)
	if err := db.step("update"); err != nil {
		return err
	}
	if db.wallets[before.ID()].Version() != before.Version() {
		return application.ErrConcurrentUpdate
	}
	db.wallets[after.ID()] = after
	return nil
}

func (r *txRepo) InsertOpening(context.Context, wager.Transaction) error { panic("not used") }

func (r *txRepo) Find(_ context.Context, provider, k, id string) (application.ExistingTransactions, error) {
	db := (*memDB)(r)
	if err := db.step("find"); err != nil {
		return application.ExistingTransactions{}, err
	}
	var found application.ExistingTransactions
	found.ByKey, found.HasByKey = db.byKey[key{provider, k}]
	found.ByExternalID, found.HasByExternalID = db.byExt[key{provider, id}]
	return found, nil
}

func (r *txRepo) Claim(_ context.Context, t wager.Transaction) (bool, error) {
	db := (*memDB)(r)
	if db.beforeClaim != nil {
		hook := db.beforeClaim
		db.beforeClaim = nil
		hook(db)
		// The hook stands for another database transaction that committed: it is
		// not part of this one, so a rollback of this one must not undo it. (This
		// unit of work has only read so far, so nothing of its own is captured.)
		db.saved = db.save()
	}
	if err := db.step("claim"); err != nil {
		return false, err
	}
	e := t.External()
	if _, taken := db.byKey[key{e.ProviderID, e.IdempotencyKey}]; taken {
		return false, nil
	}
	if _, taken := db.byExt[key{e.ProviderID, e.ExternalTransactionID}]; taken {
		return false, nil
	}
	db.store(t)
	return true, nil
}

func (db *memDB) store(t wager.Transaction) {
	e := t.External()
	db.byKey[key{e.ProviderID, e.IdempotencyKey}] = t
	db.byExt[key{e.ProviderID, e.ExternalTransactionID}] = t
	db.byID[t.ID()] = t
}

func (r *txRepo) Finish(_ context.Context, t wager.Transaction) error {
	db := (*memDB)(r)
	if err := db.step("finish"); err != nil {
		return err
	}
	if _, claimed := db.byID[t.ID()]; !claimed {
		return errors.New("finish of a transaction that was not claimed")
	}
	db.store(t)
	return nil
}

func (r *ledgerRepo) Append(_ context.Context, e wallet.LedgerEntry) error {
	db := (*memDB)(r)
	if err := db.step("ledger"); err != nil {
		return err
	}
	db.entries = append(db.entries, e)
	return nil
}

func (r *outboxRepo) Add(_ context.Context, e event.Event) error {
	db := (*memDB)(r)
	if err := db.step("outbox"); err != nil {
		return err
	}
	db.events = append(db.events, e)
	return nil
}

// --- the fixture --------------------------------------------------------------------

type recorder struct {
	mu      sync.Mutex
	handled []handled
}

type handled struct {
	kind    wager.Kind
	outcome processwager.Outcome
	took    time.Duration
}

func (r *recorder) Handled(kind wager.Kind, outcome processwager.Outcome, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handled = append(r.handled, handled{kind, outcome, took})
}

func (r *recorder) outcomes() []processwager.Outcome {
	var out []processwager.Outcome
	for _, h := range r.handled {
		out = append(out, h.outcome)
	}
	return out
}

type fixture struct {
	db   *memDB
	rec  *recorder
	logs *bytes.Buffer
	uc   *processwager.UseCase
}

func newFixture(t *testing.T, balance int64) *fixture {
	t.Helper()
	f := &fixture{db: newDB(), rec: &recorder{}, logs: &bytes.Buffer{}}
	f.db.addWallet(walletWith(t, balance))
	f.uc = processwager.New(processwager.Deps{
		UnitOfWork: f.db, Recorder: f.rec, Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
		Now: func() time.Time { return now }, NewID: uuid.New,
	})
	return f
}

func walletWith(t *testing.T, balance int64) wallet.Wallet {
	t.Helper()
	w, err := wallet.Rehydrate(wallet.State{ID: wid, PlayerID: player, Balance: brl(balance), Version: 2, LastTransactionID: uuid.New(), CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	return w
}

func betInput(amount int64) processwager.Input {
	return processwager.Input{
		Request: wager.RequestParams{
			ProviderID: "provider-a", ExternalTransactionID: "transaction-123", PlayerID: player, WalletID: wid,
			RoundID: "round-987", GameID: "fortune-chimp", Kind: wager.KindBet, Amount: brl(amount),
		},
		IdempotencyKey: "provider-a:transaction-123", CorrelationID: "corr-1", CausationID: "msg-1",
	}
}

func (f *fixture) balance() int64 { return f.db.wallets[wid].Balance().Minor() }

func types(events []event.Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type())
	}
	return out
}

// --- a new bet ----------------------------------------------------------------------

func TestANewBetIsProcessedInTheDocumentedOrder(t *testing.T) {
	f := newFixture(t, 10000)

	out, err := f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)

	require.Equal(t, []string{"find", "lock", "claim", "update", "ledger", "finish", "outbox", "outbox"}, f.db.calls,
		"stored answer first (no lock), then the wallet lock, then the claim, then the writes")
	require.Equal(t, 1, f.db.uowRuns, "one unit of work: everything commits together")

	require.False(t, out.IdempotentReplay)
	require.Equal(t, wager.StatusProcessed, out.Transaction.Status())
	result, has := out.Transaction.Result()
	require.True(t, has)
	require.Equal(t, int64(7500), result.Balance.Minor())
	require.Equal(t, int64(3), result.WalletVersion)

	require.Equal(t, int64(7500), f.balance())
	require.Len(t, f.db.entries, 1)
	entry := f.db.entries[0]
	require.Equal(t, wallet.DirectionDebit, entry.Direction())
	require.Equal(t, out.Transaction.ID(), entry.TransactionID())
	require.Equal(t, int64(10000), entry.BalanceBefore().Minor())
	require.Equal(t, int64(7500), entry.BalanceAfter().Minor())

	stored := f.db.byKey[key{"provider-a", "provider-a:transaction-123"}]
	require.Equal(t, wager.StatusProcessed, stored.Status(), "what is stored is the final state")
	require.Equal(t, out.Transaction.ID(), stored.ID())

	require.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, types(f.db.events))
	for _, e := range f.db.events {
		require.Equal(t, wid, e.AggregateID())
		require.Equal(t, "corr-1", e.CorrelationID())
		require.Equal(t, "msg-1", e.CausationID())
	}
	data, err := f.db.events[0].DataJSON()
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"transactionId":%q,"walletId":%q,"playerId":%q,"kind":"BET","origin":"EXTERNAL",
		"providerId":"provider-a","externalTransactionId":"transaction-123","roundId":"round-987","gameId":"fortune-chimp",
		"money":{"amount":"25.00","currency":"BRL"},"balance":{"amount":"75.00","currency":"BRL"},"walletVersion":3}`,
		out.Transaction.ID(), wid, player), string(data))

	require.Equal(t, []processwager.Outcome{processwager.OutcomeProcessed}, f.rec.outcomes())
	require.Equal(t, wager.KindBet, f.rec.handled[0].kind)
	require.GreaterOrEqual(t, f.rec.handled[0].took, time.Duration(0))
}

// --- replays and conflicts ----------------------------------------------------------

func TestAReplayIsAnsweredFromStorageWithTheBalanceOfTheFirstTime(t *testing.T) {
	f := newFixture(t, 10000)
	first, err := f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)

	// Other things happen to the wallet in between.
	other := betInput(1000)
	other.Request.ExternalTransactionID, other.IdempotencyKey = "transaction-999", "provider-a:transaction-999"
	_, err = f.uc.Execute(t.Context(), other)
	require.NoError(t, err)
	require.Equal(t, int64(6500), f.balance())

	f.db.calls = nil
	replay, err := f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)

	require.True(t, replay.IdempotentReplay)
	require.Equal(t, first.Transaction.ID(), replay.Transaction.ID())
	result, _ := replay.Transaction.Result()
	require.Equal(t, int64(7500), result.Balance.Minor(), "the balance seen when it was first processed, not the current 65.00")
	require.Equal(t, []string{"find"}, f.db.calls, "a replay takes no lock and writes nothing")
	require.Equal(t, int64(6500), f.balance())
	require.Len(t, f.db.entries, 2)
	require.Equal(t, 4, len(f.db.events), "and emits no events: they were written the first time")
	require.Equal(t, []processwager.Outcome{processwager.OutcomeProcessed, processwager.OutcomeProcessed, processwager.OutcomeReplayed}, f.rec.outcomes())
}

func TestTheSameKeyWithOtherContentIsAConflict(t *testing.T) {
	f := newFixture(t, 10000)
	_, err := f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)
	f.db.calls = nil

	for name, change := range map[string]func(*processwager.Input){
		"another amount": func(in *processwager.Input) { in.Request.Amount = brl(2501) },
		"another round":  func(in *processwager.Input) { in.Request.RoundID = "round-1" },
		"another game":   func(in *processwager.Input) { in.Request.GameID = "other" },
	} {
		in := betInput(2500)
		change(&in)
		_, err := f.uc.Execute(t.Context(), in)
		require.ErrorIs(t, err, processwager.ErrIdempotencyKeyConflict, name)
	}
	require.Equal(t, int64(7500), f.balance(), "a conflict applies nothing")
	require.Len(t, f.db.entries, 1)
}

func TestTheSameExternalIdUnderAnotherKeyIsADuplicateAndAppliesNothing(t *testing.T) {
	f := newFixture(t, 10000)
	_, err := f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)

	again := betInput(2500)
	again.IdempotencyKey = "a-different-key"
	_, err = f.uc.Execute(t.Context(), again)
	require.ErrorIs(t, err, processwager.ErrDuplicateExternalTransaction)
	require.Equal(t, int64(7500), f.balance())
	require.Len(t, f.db.entries, 1, "the financial operation is not applied twice")

	different := again
	different.Request.Amount = brl(1)
	_, err = f.uc.Execute(t.Context(), different)
	require.ErrorIs(t, err, processwager.ErrDuplicateExternalTransaction, "whatever its content")
}

func TestOneProvidersKeysAndIdsNeverCollideWithAnothers(t *testing.T) {
	f := newFixture(t, 10000)
	_, err := f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)

	b := betInput(2500)
	b.Request.ProviderID = "provider-b"
	out, err := f.uc.Execute(t.Context(), b)
	require.NoError(t, err, "same key and same external id, other provider")
	require.False(t, out.IdempotentReplay)
	require.Equal(t, int64(5000), f.balance(), "both were applied")
}

func TestLosingTheClaimToAConcurrentRequestAnswersWithWhatTheWinnerStored(t *testing.T) {
	t.Run("the same request: a replay", func(t *testing.T) {
		f := newFixture(t, 10000)
		var winner wager.Transaction
		f.db.beforeClaim = func(db *memDB) { winner = f.raceWith(t, betInput(2500)) }
		out, err := f.uc.Execute(t.Context(), betInput(2500))
		require.NoError(t, err)
		require.True(t, out.IdempotentReplay)
		require.Equal(t, winner.ID(), out.Transaction.ID())
		require.Equal(t, int64(7500), f.balance(), "debited once, by the winner")
		require.Len(t, f.db.entries, 1)
		require.NotContains(t, f.db.calls, "update", "the loser writes nothing")
		require.NotContains(t, f.db.calls, "finish")
		require.NotContains(t, f.db.calls, "outbox")
	})
	t.Run("the same key with other content: a conflict", func(t *testing.T) {
		f := newFixture(t, 10000)
		f.db.beforeClaim = func(db *memDB) { f.raceWith(t, betInput(2500)) }
		_, err := f.uc.Execute(t.Context(), betInput(9999))
		require.ErrorIs(t, err, processwager.ErrIdempotencyKeyConflict)
		require.Equal(t, int64(7500), f.balance())
	})
	t.Run("the same external id under another key: a duplicate", func(t *testing.T) {
		f := newFixture(t, 10000)
		f.db.beforeClaim = func(db *memDB) { f.raceWith(t, betInput(2500)) }
		in := betInput(2500)
		in.IdempotencyKey = "another-key"
		_, err := f.uc.Execute(t.Context(), in)
		require.ErrorIs(t, err, processwager.ErrDuplicateExternalTransaction)
		require.Equal(t, int64(7500), f.balance())
	})
}

// raceWith stands in for a concurrent request that wins the claim: it processes the input
// completely, as another database transaction that committed first.
func (f *fixture) raceWith(t *testing.T, in processwager.Input) wager.Transaction {
	t.Helper()
	request, err := wager.NewRequest(in.Request)
	require.NoError(t, err)
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request, IdempotencyKey: in.IdempotencyKey, CorrelationID: "other", At: now})
	require.NoError(t, err)
	s, err := tx.Settle(f.db.wallets[wid], uuid.New(), now)
	require.NoError(t, err)
	f.db.wallets[wid] = s.Wallet
	f.db.entries = append(f.db.entries, s.Entry)
	f.db.store(s.Transaction)
	return s.Transaction
}

// --- rejections ---------------------------------------------------------------------

func TestAnInsufficientBalanceIsAStoredFinalReplayableRejection(t *testing.T) {
	f := newFixture(t, 5000)
	out, err := f.uc.Execute(t.Context(), betInput(8000))
	require.NoError(t, err, "a Rejection is an outcome, not an error")

	require.Equal(t, wager.StatusRejected, out.Transaction.Status())
	require.Equal(t, wager.FailureInsufficientFunds, out.Transaction.FailureCode())
	require.Equal(t, int64(5000), f.balance())
	require.Empty(t, f.db.entries, "a rejection writes no ledger entry")
	require.Equal(t, []string{"find", "lock", "claim", "finish", "outbox"}, f.db.calls, "no wallet update")
	require.Equal(t, []string{"WagerTransactionRejected"}, types(f.db.events))
	data, err := f.db.events[0].DataJSON()
	require.NoError(t, err)
	require.JSONEq(t, fmt.Sprintf(`{"transactionId":%q,"walletId":%q,"playerId":%q,"kind":"BET","providerId":"provider-a",
		"externalTransactionId":"transaction-123","roundId":"round-987","gameId":"fortune-chimp",
		"money":{"amount":"80.00","currency":"BRL"},"failureCode":"INSUFFICIENT_FUNDS"}`, out.Transaction.ID(), wid, player), string(data))

	stored := f.db.byKey[key{"provider-a", "provider-a:transaction-123"}]
	require.Equal(t, wager.StatusRejected, stored.Status(), "persisted")

	// The wallet is topped up in the meantime; the answer to the same request does not change.
	f.db.addWallet(walletWith(t, 99999))
	replay, err := f.uc.Execute(t.Context(), betInput(8000))
	require.NoError(t, err)
	require.True(t, replay.IdempotentReplay)
	require.Equal(t, wager.FailureInsufficientFunds, replay.Transaction.FailureCode(), "terminal: sending again does not retry the bet")
	require.Equal(t, []processwager.Outcome{processwager.OutcomeRejected, processwager.OutcomeReplayed}, f.rec.outcomes())
}

func TestAnUnknownWalletIsStoredWithTheRequestedIdAndNoWallet(t *testing.T) {
	f := newFixture(t, 10000)
	in := betInput(2500)
	missing := uuid.New()
	in.Request.WalletID = missing

	out, err := f.uc.Execute(t.Context(), in)
	require.NoError(t, err)
	require.Equal(t, wager.StatusRejected, out.Transaction.Status())
	require.Equal(t, wager.FailureWalletNotFound, out.Transaction.FailureCode())
	require.Equal(t, uuid.Nil, out.Transaction.WalletID(), "there is no such wallet")
	require.Equal(t, missing, out.Transaction.RequestedWalletID(), "but what was asked for is kept")

	require.Equal(t, []string{"find", "lock", "claim", "outbox"}, f.db.calls, "claimed in its final state: no update afterwards")
	require.Equal(t, []string{"WagerTransactionRejected"}, types(f.db.events))
	require.Equal(t, missing, f.db.events[0].AggregateID())
	require.Equal(t, int64(10000), f.balance())

	replay, err := f.uc.Execute(t.Context(), in)
	require.NoError(t, err)
	require.True(t, replay.IdempotentReplay)
	require.Equal(t, wager.FailureWalletNotFound, replay.Transaction.FailureCode())
}

func TestAnUnknownWalletThatLosesTheClaimAnswersWithTheStoredOne(t *testing.T) {
	f := newFixture(t, 10000)
	in := betInput(2500)
	in.Request.WalletID = uuid.New()
	f.db.beforeClaim = func(db *memDB) {
		request, err := wager.NewRequest(in.Request)
		require.NoError(t, err)
		tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request, IdempotencyKey: in.IdempotencyKey, CorrelationID: "other", At: now})
		require.NoError(t, err)
		rejected, err := tx.Reject(wager.FailureWalletNotFound, now)
		require.NoError(t, err)
		db.store(rejected)
	}
	out, err := f.uc.Execute(t.Context(), in)
	require.NoError(t, err)
	require.True(t, out.IdempotentReplay)
	require.Empty(t, f.db.events, "the winner wrote the event")
}

func TestAnotherPlayersWalletAndAnotherCurrencyAreStoredRejections(t *testing.T) {
	t.Run("player mismatch", func(t *testing.T) {
		f := newFixture(t, 10000)
		in := betInput(2500)
		in.Request.PlayerID = uuid.New()
		out, err := f.uc.Execute(t.Context(), in)
		require.NoError(t, err)
		require.Equal(t, wager.FailurePlayerMismatch, out.Transaction.FailureCode())
		require.Equal(t, int64(10000), f.balance())
		require.Empty(t, f.db.entries)
		data, err := f.db.events[0].DataJSON()
		require.NoError(t, err)
		require.Contains(t, string(data), `"failureCode":"PLAYER_MISMATCH"`, "the event says why")
	})
	t.Run("currency mismatch", func(t *testing.T) {
		f := newFixture(t, 10000)
		in := betInput(2500)
		usd, err := money.FromMinor(2500, money.USD)
		require.NoError(t, err)
		in.Request.Amount = usd
		out, err := f.uc.Execute(t.Context(), in)
		require.NoError(t, err)
		require.Equal(t, wager.FailureCurrencyMismatch, out.Transaction.FailureCode())
		require.Equal(t, int64(10000), f.balance())
	})
}

// --- refusals before the database -----------------------------------------------------

func TestInvalidInputNeverReachesTheDatabase(t *testing.T) {
	cases := map[string]func(*processwager.Input){
		"an opening":           func(in *processwager.Input) { in.Request.Kind = wager.KindOpening },
		"a zero bet":           func(in *processwager.Input) { in.Request.Amount = brl(0) },
		"no provider":          func(in *processwager.Input) { in.Request.ProviderID = "" },
		"no external id":       func(in *processwager.Input) { in.Request.ExternalTransactionID = "" },
		"no key":               func(in *processwager.Input) { in.IdempotencyKey = "" },
		"a key with a space":   func(in *processwager.Input) { in.IdempotencyKey = "two words" },
		"no correlation id":    func(in *processwager.Input) { in.CorrelationID = "" },
		"a reference on a bet": func(in *processwager.Input) { in.Request.ReferenceExternalTransactionID = "x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 10000)
			in := betInput(2500)
			mutate(&in)
			_, err := f.uc.Execute(t.Context(), in)
			require.ErrorIs(t, err, processwager.ErrInvalidInput)
			require.Zero(t, f.db.uowRuns)
			require.Equal(t, []processwager.Outcome{processwager.OutcomeInvalid}, f.rec.outcomes())
		})
	}
}

func TestKindsNotProcessedYetAreRefusedBeforeTheDatabase(t *testing.T) {
	for _, kind := range []wager.Kind{wager.KindWin, wager.KindLoss, wager.KindRefund, wager.KindRollback} {
		f := newFixture(t, 10000)
		in := betInput(2500)
		in.Request.Kind = kind
		switch kind {
		case wager.KindLoss:
			in.Request.Amount = brl(0)
		case wager.KindRefund, wager.KindRollback:
			in.Request.ReferenceExternalTransactionID = "transaction-100"
		}
		_, err := f.uc.Execute(t.Context(), in)
		require.ErrorIs(t, err, processwager.ErrUnsupportedKind, kind)
		require.Zero(t, f.db.uowRuns)
		require.Equal(t, []processwager.Outcome{processwager.OutcomeUnsupported}, f.rec.outcomes())
	}
}

// --- failures leave nothing behind --------------------------------------------------

func TestAFailureAtAnyStepRollsEverythingBack(t *testing.T) {
	for _, step := range []string{"find", "lock", "claim", "update", "ledger", "finish", "outbox"} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t, 10000)
			f.db.failAt = step
			_, err := f.uc.Execute(t.Context(), betInput(2500))
			require.ErrorIs(t, err, errInjected)

			require.Equal(t, int64(10000), f.balance())
			require.Empty(t, f.db.entries)
			require.Empty(t, f.db.events)
			require.Empty(t, f.db.byKey, "not even the claim survives")
			require.Equal(t, []processwager.Outcome{processwager.OutcomeError}, f.rec.outcomes())

			// And the request, sent again, is processed as if it were new.
			f.db.failAt = ""
			out, err := f.uc.Execute(t.Context(), betInput(2500))
			require.NoError(t, err)
			require.False(t, out.IdempotentReplay)
			require.Equal(t, int64(7500), f.balance())
		})
	}
}

func TestATransientErrorIsReportedAsSuchAndStoresNothing(t *testing.T) {
	f := newFixture(t, 10000)
	f.db = newDB()
	f.db.addWallet(walletWith(t, 10000))
	uow := transientUnitOfWork{inner: f.db}
	uc := processwager.New(processwager.Deps{UnitOfWork: uow, Recorder: f.rec, Now: func() time.Time { return now }, NewID: uuid.New})

	_, err := uc.Execute(t.Context(), betInput(2500))
	require.ErrorIs(t, err, application.ErrTemporarilyUnavailable)
	require.Equal(t, []processwager.Outcome{processwager.OutcomeUnavailable}, f.rec.outcomes())
	require.Empty(t, f.db.byKey)
}

type transientUnitOfWork struct{ inner *memDB }

func (transientUnitOfWork) Do(context.Context, func(context.Context, application.Repositories) error) error {
	return fmt.Errorf("%w: connection refused", application.ErrTemporarilyUnavailable)
}

// --- observability ------------------------------------------------------------------

func TestTheLogCarriesTheIdentifiersAndNoMoneyOrKeys(t *testing.T) {
	f := newFixture(t, 10000)
	out, err := f.uc.Execute(t.Context(), betInput(2599))
	require.NoError(t, err)

	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &parsed))
		if parsed["msg"] == "wager request handled" {
			entry = parsed
		}
	}
	require.NotNil(t, entry, f.logs.String())
	require.Equal(t, "corr-1", entry["correlationId"])
	require.Equal(t, "provider-a", entry["providerId"])
	require.Equal(t, wid.String(), entry["walletId"])
	require.Equal(t, out.Transaction.ID().String(), entry["transactionId"])
	require.Equal(t, "processed", entry["outcome"])
	require.Equal(t, "BET", entry["kind"])

	for _, secret := range []string{"25.99", "2599", "provider-a:transaction-123", "fortune-chimp", "round-987"} {
		require.NotContains(t, f.logs.String(), secret, "the log must not carry payloads, amounts or the idempotency key")
	}
}

func TestEveryRequestIsMeasuredExactlyOnce(t *testing.T) {
	f := newFixture(t, 10000)
	_, _ = f.uc.Execute(t.Context(), betInput(2500))
	_, _ = f.uc.Execute(t.Context(), betInput(2500))
	conflict := betInput(1)
	_, _ = f.uc.Execute(t.Context(), conflict)
	bad := betInput(2500)
	bad.IdempotencyKey = ""
	_, _ = f.uc.Execute(t.Context(), bad)
	require.Equal(t, []processwager.Outcome{
		processwager.OutcomeProcessed, processwager.OutcomeReplayed, processwager.OutcomeKeyConflict, processwager.OutcomeInvalid,
	}, f.rec.outcomes())
}

// --- the ordering rule ---------------------------------------------------------------

func TestTheStoredAnswerIsLookedUpBeforeAnyLockIsTaken(t *testing.T) {
	f := newFixture(t, 10000)
	_, err := f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)
	f.db.calls = nil

	// A request that will be answered from storage never gets as far as the lock.
	_, err = f.uc.Execute(t.Context(), betInput(2500))
	require.NoError(t, err)
	require.NotContains(t, f.db.calls, "lock")
	f.db.calls = nil
	again := betInput(2500)
	again.IdempotencyKey = "other-key"
	_, err = f.uc.Execute(t.Context(), again)
	require.Error(t, err)
	require.NotContains(t, f.db.calls, "lock")
}
