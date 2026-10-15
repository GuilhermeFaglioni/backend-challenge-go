package openwallet_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

var now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	return m
}

// recorder keeps what the fake repositories were asked to do, in order.
type recorder struct {
	calls   []string
	wallets []wallet.Wallet
	txs     []wager.Transaction
	entries []wallet.LedgerEntry
	events  []event.Event

	failOn     string // the call that returns failWith
	failWith   error
	uowCalls   int
	outcomes   []openwallet.Outcome
	rolledBack bool
}

func (r *recorder) Do(ctx context.Context, fn func(context.Context, application.Repositories) error) error {
	r.uowCalls++
	staged := *r
	err := fn(ctx, r)
	if err != nil {
		// A real unit of work rolls back: forget everything the function did.
		r.calls, r.wallets, r.txs, r.entries, r.events = staged.calls, staged.wallets, staged.txs, staged.entries, staged.events
		r.rolledBack = true
	}
	return err
}

func (r *recorder) Opened(o openwallet.Outcome) { r.outcomes = append(r.outcomes, o) }

func (r *recorder) step(name string) error {
	r.calls = append(r.calls, name)
	if r.failOn == name {
		return r.failWith
	}
	return nil
}

func (r *recorder) Wallets() application.WalletRepository           { return (*walletRepo)(r) }
func (r *recorder) Transactions() application.TransactionRepository { return (*txRepo)(r) }
func (r *recorder) Ledger() application.LedgerRepository            { return (*ledgerRepo)(r) }
func (r *recorder) Outbox() application.OutboxRepository            { return (*outboxRepo)(r) }
func (r *recorder) Inbox() application.InboxRepository              { panic("not used") }

type (
	walletRepo recorder
	txRepo     recorder
	ledgerRepo recorder
	outboxRepo recorder
)

// Opening a wallet never reads or locks one, and never touches the idempotency of an
// external operation: the fakes fail loudly if it starts to.
func (w *walletRepo) Lock(context.Context, uuid.UUID) (wallet.Wallet, error) {
	panic("openwallet must not lock a wallet")
}

func (w *walletRepo) Update(context.Context, wallet.Wallet, wallet.Wallet) error {
	panic("openwallet must not update a wallet")
}

func (t *txRepo) Find(context.Context, string, string, string) (application.ExistingTransactions, error) {
	panic("openwallet looks up no idempotency key")
}

func (t *txRepo) Claim(context.Context, wager.Transaction) (bool, error) {
	panic("openwallet claims no idempotency key")
}

func (t *txRepo) Finish(context.Context, wager.Transaction) error {
	panic("openwallet records the opening in its final state")
}

func (w *walletRepo) Insert(_ context.Context, v wallet.Wallet) error {
	r := (*recorder)(w)
	if err := r.step("wallet"); err != nil {
		return err
	}
	r.wallets = append(r.wallets, v)
	return nil
}

func (t *txRepo) InsertOpening(_ context.Context, v wager.Transaction) error {
	r := (*recorder)(t)
	if err := r.step("transaction"); err != nil {
		return err
	}
	r.txs = append(r.txs, v)
	return nil
}

func (l *ledgerRepo) Append(_ context.Context, v wallet.LedgerEntry) error {
	r := (*recorder)(l)
	if err := r.step("ledger"); err != nil {
		return err
	}
	r.entries = append(r.entries, v)
	return nil
}

func (o *outboxRepo) Add(_ context.Context, v event.Event) error {
	r := (*recorder)(o)
	if err := r.step("outbox"); err != nil {
		return err
	}
	r.events = append(r.events, v)
	return nil
}

func newUseCase(r *recorder) *openwallet.UseCase {
	next := byte(0)
	return openwallet.New(openwallet.Deps{
		UnitOfWork: r,
		Recorder:   r,
		Now:        func() time.Time { return now },
		NewID: func() uuid.UUID {
			next++
			return uuid.UUID{15: next}
		},
	})
}

func input(t *testing.T, minor int64) openwallet.Input {
	t.Helper()
	return openwallet.Input{PlayerID: uuid.New(), InitialBalance: brl(t, minor), CorrelationID: "corr-1"}
}

// --- a positive balance -----------------------------------------------------------

func TestAPositiveBalanceWritesWalletTransactionEntryAndBothEvents(t *testing.T) {
	r := &recorder{}
	in := input(t, 5000)

	w, err := newUseCase(r).Execute(t.Context(), in)
	require.NoError(t, err)

	require.Equal(t, []string{"wallet", "transaction", "ledger", "outbox", "outbox"}, r.calls,
		"the wallet first (the others point at it), then what explains its balance, then the events")
	require.Equal(t, 1, r.uowCalls, "everything in one unit of work")

	require.Len(t, r.wallets, 1)
	stored := r.wallets[0]
	require.Equal(t, w, stored, "what is returned is what was stored")
	require.Equal(t, in.PlayerID, stored.PlayerID())
	require.Equal(t, int64(5000), stored.Balance().Minor())
	require.Equal(t, int64(1), stored.Version())

	require.Len(t, r.txs, 1)
	tx := r.txs[0]
	require.Equal(t, wager.KindOpening, tx.Kind())
	require.Equal(t, wager.StatusProcessed, tx.Status())
	require.Equal(t, stored.ID(), tx.WalletID())
	require.Equal(t, "corr-1", tx.CorrelationID())
	last, has := stored.LastTransactionID()
	require.True(t, has)
	require.Equal(t, tx.ID(), last, "the wallet's last transaction is the opening")

	require.Len(t, r.entries, 1)
	entry := r.entries[0]
	require.Equal(t, wallet.DirectionCredit, entry.Direction())
	require.Equal(t, tx.ID(), entry.TransactionID())
	require.Equal(t, stored.ID(), entry.WalletID())
	require.Equal(t, int64(0), entry.BalanceBefore().Minor())
	require.Equal(t, int64(5000), entry.BalanceAfter().Minor())

	require.Len(t, r.events, 2)
	require.Equal(t, "WagerTransactionProcessed", r.events[0].Type())
	require.Equal(t, "WalletBalanceChanged", r.events[1].Type())
	for _, e := range r.events {
		require.Equal(t, stored.ID(), e.AggregateID())
		require.Equal(t, "corr-1", e.CorrelationID())
		require.Equal(t, now, e.OccurredAt())
	}
	require.NotEqual(t, r.events[0].EventID(), r.events[1].EventID())

	changed, ok := r.events[1].Data().(event.WalletBalanceChangedData)
	require.True(t, ok)
	require.Equal(t, entry.ID() != uuid.Nil, true)
	require.Equal(t, tx.ID(), changed.TransactionID)
	require.Equal(t, int64(1), changed.WalletVersion)
	require.Equal(t, int64(5000), changed.BalanceAfter.Minor())

	require.Equal(t, []openwallet.Outcome{openwallet.OutcomeCreated}, r.outcomes)
}

// --- a zero balance ---------------------------------------------------------------

func TestAZeroBalanceCreatesOnlyTheWallet(t *testing.T) {
	r := &recorder{}
	w, err := newUseCase(r).Execute(t.Context(), input(t, 0))
	require.NoError(t, err)

	require.Equal(t, []string{"wallet"}, r.calls, "no opening, no entry, no events")
	require.Empty(t, r.txs)
	require.Empty(t, r.entries)
	require.Empty(t, r.events)
	require.True(t, w.Balance().IsZero())
	require.Equal(t, int64(1), w.Version())
	_, has := w.LastTransactionID()
	require.False(t, has)
	require.Equal(t, []openwallet.Outcome{openwallet.OutcomeCreated}, r.outcomes)
}

// --- refusals ---------------------------------------------------------------------

func TestInvalidInputNeverReachesTheDatabase(t *testing.T) {
	for name, mutate := range map[string]func(*openwallet.Input){
		"nil player":          func(in *openwallet.Input) { in.PlayerID = uuid.Nil },
		"uninitialized money": func(in *openwallet.Input) { in.InitialBalance = money.Money{} },
		"negative balance":    func(in *openwallet.Input) { in.InitialBalance, _ = money.FromMinor(-1, money.BRL) },
		"no correlation":      func(in *openwallet.Input) { in.CorrelationID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			in := input(t, 100)
			mutate(&in)
			_, err := newUseCase(r).Execute(t.Context(), in)
			require.ErrorIs(t, err, openwallet.ErrInvalidInput)
			require.Zero(t, r.uowCalls, "nothing is opened for input that cannot work")
			require.Equal(t, []openwallet.Outcome{openwallet.OutcomeInvalid}, r.outcomes)
		})
	}
}

func TestAnEmptyWalletStillNeedsACorrelationId(t *testing.T) {
	r := &recorder{}
	in := input(t, 0)
	in.CorrelationID = ""
	_, err := newUseCase(r).Execute(t.Context(), in)
	require.ErrorIs(t, err, openwallet.ErrInvalidInput, "no transaction or event would catch it, so the use case does")
	require.Zero(t, r.uowCalls)
}

func TestAnExistingWalletIsATypedConflictWithItsId(t *testing.T) {
	existing := uuid.New()
	r := &recorder{failOn: "wallet", failWith: &application.WalletAlreadyExistsError{WalletID: existing}}

	_, err := newUseCase(r).Execute(t.Context(), input(t, 5000))
	require.ErrorIs(t, err, application.ErrWalletAlreadyExists)
	var conflict *application.WalletAlreadyExistsError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, existing, conflict.WalletID)
	require.Equal(t, []openwallet.Outcome{openwallet.OutcomeConflict}, r.outcomes)
	require.Empty(t, r.events, "a conflict writes nothing")
}

func TestAnyOtherFailureIsReportedAsIsAndCounted(t *testing.T) {
	boom := errors.New("boom")
	for _, step := range []string{"wallet", "transaction", "ledger", "outbox"} {
		t.Run(step, func(t *testing.T) {
			r := &recorder{failOn: step, failWith: boom}
			_, err := newUseCase(r).Execute(t.Context(), input(t, 5000))
			require.ErrorIs(t, err, boom)
			require.NotErrorIs(t, err, openwallet.ErrInvalidInput)
			require.Equal(t, []openwallet.Outcome{openwallet.OutcomeError}, r.outcomes)
			require.Empty(t, r.wallets, "nothing of a failed attempt is left")
			require.Empty(t, r.txs)
			require.Empty(t, r.entries)
			require.Empty(t, r.events)
		})
	}
}

func TestEachCallIsCountedExactlyOnce(t *testing.T) {
	r := &recorder{}
	uc := newUseCase(r)
	_, _ = uc.Execute(t.Context(), input(t, 100))
	_, _ = uc.Execute(t.Context(), input(t, 0))
	require.Len(t, r.outcomes, 2)
}
