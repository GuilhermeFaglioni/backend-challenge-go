package wager_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

var at = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	return m
}

// mustBRL is brl for table fixtures built where no *testing.T is at hand.
func mustBRL(minor int64) money.Money {
	m, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		panic(err)
	}
	return m
}

func openingParams(t *testing.T) wager.OpeningParams {
	t.Helper()
	return wager.OpeningParams{
		ID: uuid.New(), WalletID: uuid.New(), PlayerID: uuid.New(),
		Amount: brl(t, 5000), CorrelationID: "corr-1", At: at,
	}
}

func pendingOpening(t *testing.T) wager.Transaction {
	t.Helper()
	tx, err := wager.NewOpening(openingParams(t))
	require.NoError(t, err)
	return tx
}

// --- the state machine -------------------------------------------------------------

func TestTheStateMachineAllowsOnlyTheDocumentedTransitions(t *testing.T) {
	all := []wager.Status{wager.StatusPending, wager.StatusPendingReference, wager.StatusProcessed, wager.StatusRejected, wager.StatusFailed}
	allowed := map[wager.Status][]wager.Status{
		wager.StatusPending:          {wager.StatusProcessed, wager.StatusRejected, wager.StatusPendingReference, wager.StatusFailed},
		wager.StatusPendingReference: {wager.StatusProcessed, wager.StatusRejected, wager.StatusFailed},
	}
	for _, from := range all {
		for _, to := range all {
			want := false
			for _, ok := range allowed[from] {
				want = want || ok == to
			}
			require.Equal(t, want, from.CanTransitionTo(to), "%s -> %s", from, to)
		}
	}
}

func TestOnlyProcessedRejectedAndFailedAreTerminal(t *testing.T) {
	for status, terminal := range map[wager.Status]bool{
		wager.StatusPending: false, wager.StatusPendingReference: false,
		wager.StatusProcessed: true, wager.StatusRejected: true, wager.StatusFailed: true,
	} {
		require.Equal(t, terminal, status.IsTerminal(), status)
	}
	require.False(t, wager.Status("DONE").IsTerminal(), "an unknown status is not terminal, it is not a status")
	require.False(t, wager.Status("DONE").IsValid())
	require.False(t, wager.Status("").IsValid())
	require.False(t, wager.Status("DONE").CanTransitionTo(wager.StatusProcessed))
}

// --- the opening -------------------------------------------------------------------

func TestAnOpeningStartsPendingWithWhatItNeeds(t *testing.T) {
	p := openingParams(t)
	tx, err := wager.NewOpening(p)
	require.NoError(t, err)

	require.True(t, tx.IsValid())
	require.Equal(t, p.ID, tx.ID())
	require.Equal(t, wager.KindOpening, tx.Kind())
	require.Equal(t, wager.OriginInternal, tx.Origin())
	require.Equal(t, p.WalletID, tx.WalletID())
	require.Equal(t, p.PlayerID, tx.PlayerID())
	require.Equal(t, p.Amount, tx.Amount())
	require.Equal(t, wager.StatusPending, tx.Status())
	require.Equal(t, "corr-1", tx.CorrelationID())
	require.Equal(t, at, tx.CreatedAt())
	require.Equal(t, at, tx.UpdatedAt())
	_, has := tx.Result()
	require.False(t, has)
	_, has = tx.ProcessedAt()
	require.False(t, has)
	require.Empty(t, tx.FailureCode())
}

func TestAnOpeningRefusesWhatItCannotWorkWith(t *testing.T) {
	cases := map[string]struct {
		mutate func(*wager.OpeningParams)
		want   error
	}{
		"nil id":          {func(p *wager.OpeningParams) { p.ID = uuid.Nil }, wager.ErrInvalidID},
		"nil wallet":      {func(p *wager.OpeningParams) { p.WalletID = uuid.Nil }, wager.ErrInvalidID},
		"nil player":      {func(p *wager.OpeningParams) { p.PlayerID = uuid.Nil }, wager.ErrInvalidID},
		"zero amount":     {func(p *wager.OpeningParams) { p.Amount = money.Money{} }, wager.ErrInvalidAmount},
		"no money at all": {func(p *wager.OpeningParams) { p.Amount, _ = money.Zero(money.BRL) }, wager.ErrInvalidAmount},
		"negative":        {func(p *wager.OpeningParams) { p.Amount, _ = money.FromMinor(-1, money.BRL) }, wager.ErrInvalidAmount},
		"no correlation":  {func(p *wager.OpeningParams) { p.CorrelationID = "" }, wager.ErrInvalidCorrelation},
		"zero time":       {func(p *wager.OpeningParams) { p.At = time.Time{} }, wager.ErrInvalidTime},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := openingParams(t)
			c.mutate(&p)
			tx, err := wager.NewOpening(p)
			require.ErrorIs(t, err, c.want)
			require.False(t, tx.IsValid())
		})
	}
}

func TestProcessingAnOpeningRecordsWhatItProduced(t *testing.T) {
	tx := pendingOpening(t)
	later := at.Add(time.Second)

	done, err := tx.Process(wager.Result{Balance: tx.Amount(), WalletVersion: 1}, later)
	require.NoError(t, err)

	require.Equal(t, wager.StatusProcessed, done.Status())
	result, has := done.Result()
	require.True(t, has)
	require.Equal(t, tx.Amount(), result.Balance)
	require.Equal(t, int64(1), result.WalletVersion)
	processed, has := done.ProcessedAt()
	require.True(t, has)
	require.Equal(t, later, processed)
	require.Equal(t, later, done.UpdatedAt())
	require.Equal(t, at, done.CreatedAt())

	require.Equal(t, wager.StatusPending, tx.Status(), "the transaction it was made on does not change")
}

func TestAnOpeningResultMustBeTheFundedWalletAtVersionOne(t *testing.T) {
	tx := pendingOpening(t)
	usd, err := money.FromMinor(5000, money.USD)
	require.NoError(t, err)
	for name, result := range map[string]wager.Result{
		"another balance":  {Balance: brl(t, 4999), WalletVersion: 1},
		"another version":  {Balance: tx.Amount(), WalletVersion: 2},
		"zero version":     {Balance: tx.Amount(), WalletVersion: 0},
		"another currency": {Balance: usd, WalletVersion: 1},
		"no balance":       {WalletVersion: 1},
	} {
		got, err := tx.Process(result, at.Add(time.Second))
		require.ErrorIs(t, err, wager.ErrInvalidResult, name)
		require.False(t, got.IsValid(), name)
	}
}

func TestFailingAnOpeningRecordsThePermanentFailure(t *testing.T) {
	failed, err := pendingOpening(t).Fail(at.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, wager.StatusFailed, failed.Status())
	require.Equal(t, wager.FailureInternalPermanent, failed.FailureCode())
	_, has := failed.Result()
	require.False(t, has, "a failure moved no money")
	_, has = failed.ProcessedAt()
	require.True(t, has)
}

func TestATerminalTransactionAcceptsNoTransition(t *testing.T) {
	tx := pendingOpening(t)
	processed, err := tx.Process(wager.Result{Balance: tx.Amount(), WalletVersion: 1}, at.Add(time.Second))
	require.NoError(t, err)
	failed, err := tx.Fail(at.Add(time.Second))
	require.NoError(t, err)

	for name, terminal := range map[string]wager.Transaction{"processed": processed, "failed": failed} {
		_, err := terminal.Process(wager.Result{Balance: tx.Amount(), WalletVersion: 1}, at.Add(time.Minute))
		require.ErrorIs(t, err, wager.ErrInvalidTransition, name+" processed again")
		_, err = terminal.Fail(at.Add(time.Minute))
		require.ErrorIs(t, err, wager.ErrInvalidTransition, name+" failed")
	}
}

func TestATransitionNeedsATimeThatDoesNotGoBack(t *testing.T) {
	tx := pendingOpening(t)
	result := wager.Result{Balance: tx.Amount(), WalletVersion: 1}

	_, err := tx.Process(result, time.Time{})
	require.ErrorIs(t, err, wager.ErrInvalidTime)
	_, err = tx.Process(result, at.Add(-time.Second))
	require.ErrorIs(t, err, wager.ErrInvalidTime)
	_, err = tx.Fail(time.Time{})
	require.ErrorIs(t, err, wager.ErrInvalidTime)
	_, err = tx.Fail(at.Add(-time.Second))
	require.ErrorIs(t, err, wager.ErrInvalidTime)
}

func TestTheZeroTransactionRefusesEverything(t *testing.T) {
	var tx wager.Transaction
	require.False(t, tx.IsValid())
	_, err := tx.Process(wager.Result{}, at)
	require.ErrorIs(t, err, wager.ErrUninitialized)
	_, err = tx.Fail(at)
	require.ErrorIs(t, err, wager.ErrUninitialized)
}

func TestTimesAreKeptInUTC(t *testing.T) {
	p := openingParams(t)
	p.At = time.Date(2026, 10, 14, 9, 0, 0, 0, time.FixedZone("BRT", -3*60*60))
	tx, err := wager.NewOpening(p)
	require.NoError(t, err)
	require.Equal(t, time.UTC, tx.CreatedAt().Location())
	require.True(t, p.At.Equal(tx.CreatedAt()))
}

// --- rehydration -------------------------------------------------------------------

func processedState(t *testing.T) wager.State {
	t.Helper()
	processed := at.Add(time.Second)
	walletID := uuid.New()
	return wager.State{
		RequestedWalletID: walletID, WalletID: walletID,
		ID: uuid.New(), Kind: wager.KindOpening, PlayerID: uuid.New(),
		Amount: brl(t, 5000), Status: wager.StatusProcessed,
		Result:        &wager.Result{Balance: brl(t, 5000), WalletVersion: 1},
		CorrelationID: "corr-1", CreatedAt: at, UpdatedAt: processed, ProcessedAt: &processed,
	}
}

func TestRehydratingRestoresTheStateAsIs(t *testing.T) {
	s := processedState(t)
	tx, err := wager.Rehydrate(s)
	require.NoError(t, err)
	require.Equal(t, s.ID, tx.ID())
	require.Equal(t, wager.StatusProcessed, tx.Status())
	result, has := tx.Result()
	require.True(t, has)
	require.Equal(t, *s.Result, result)
	processed, has := tx.ProcessedAt()
	require.True(t, has)
	require.Equal(t, *s.ProcessedAt, processed)

	t.Run("a pending one", func(t *testing.T) {
		s := processedState(t)
		s.Status, s.Result, s.ProcessedAt, s.UpdatedAt = wager.StatusPending, nil, nil, s.CreatedAt
		tx, err := wager.Rehydrate(s)
		require.NoError(t, err)
		require.Equal(t, wager.StatusPending, tx.Status())
	})
	t.Run("a failed one", func(t *testing.T) {
		s := processedState(t)
		s.Status, s.Result, s.FailureCode = wager.StatusFailed, nil, wager.FailureInternalPermanent
		tx, err := wager.Rehydrate(s)
		require.NoError(t, err)
		require.Equal(t, wager.FailureInternalPermanent, tx.FailureCode())
	})
}

func TestRehydratedTimesAreInUTC(t *testing.T) {
	brt := time.FixedZone("BRT", -3*60*60)
	s := processedState(t)
	created, updated := at.In(brt), at.Add(time.Second).In(brt)
	s.CreatedAt, s.UpdatedAt, s.ProcessedAt = created, updated, &updated
	tx, err := wager.Rehydrate(s)
	require.NoError(t, err)
	require.Equal(t, time.UTC, tx.CreatedAt().Location())
	require.Equal(t, time.UTC, tx.UpdatedAt().Location())
	processed, _ := tx.ProcessedAt()
	require.Equal(t, time.UTC, processed.Location())
}

func TestRehydratingRefusesWhatTheDatabaseCouldNeverHold(t *testing.T) {
	cases := map[string]struct {
		mutate func(*wager.State)
		want   error
	}{
		"nil id":         {func(s *wager.State) { s.ID = uuid.Nil }, wager.ErrInvalidID},
		"nil wallet":     {func(s *wager.State) { s.WalletID = uuid.Nil }, wager.ErrInvalidID},
		"nil player":     {func(s *wager.State) { s.PlayerID = uuid.Nil }, wager.ErrInvalidID},
		"unknown kind":   {func(s *wager.State) { s.Kind = "TRANSFER" }, wager.ErrInvalidKind},
		"no kind":        {func(s *wager.State) { s.Kind = "" }, wager.ErrInvalidKind},
		"bad amount":     {func(s *wager.State) { s.Amount = mustBRL(0) }, wager.ErrInvalidAmount},
		"unknown status": {func(s *wager.State) { s.Status = "DONE" }, wager.ErrInvalidStatus},
		"an opening is never rejected": {func(s *wager.State) {
			s.Status, s.Result, s.FailureCode = wager.StatusRejected, nil, "INSUFFICIENT_FUNDS"
		}, wager.ErrInvalidStatus},
		"an opening never waits for a reference": {func(s *wager.State) {
			s.Status, s.Result, s.ProcessedAt, s.UpdatedAt = wager.StatusPendingReference, nil, nil, s.CreatedAt
		}, wager.ErrInvalidStatus},
		"processed without a result":       {func(s *wager.State) { s.Result = nil }, wager.ErrInvalidResult},
		"processed with a failure code":    {func(s *wager.State) { s.FailureCode = wager.FailureInternalPermanent }, wager.ErrInvalidResult},
		"processed with another balance":   {func(s *wager.State) { s.Result = &wager.Result{Balance: mustBRL(1), WalletVersion: 1} }, wager.ErrInvalidResult},
		"processed without a time":         {func(s *wager.State) { s.ProcessedAt = nil }, wager.ErrInvalidTime},
		"pending with a result":            {func(s *wager.State) { s.Status, s.ProcessedAt, s.UpdatedAt = wager.StatusPending, nil, s.CreatedAt }, wager.ErrInvalidResult},
		"failed without a failure code":    {func(s *wager.State) { s.Status, s.Result = wager.StatusFailed, nil }, wager.ErrInvalidResult},
		"failed with a result":             {func(s *wager.State) { s.Status, s.FailureCode = wager.StatusFailed, wager.FailureInternalPermanent }, wager.ErrInvalidResult},
		"failed with another failure code": {func(s *wager.State) { s.Status, s.Result, s.FailureCode = wager.StatusFailed, nil, "OTHER" }, wager.ErrInvalidResult},
		"no correlation":                   {func(s *wager.State) { s.CorrelationID = "" }, wager.ErrInvalidCorrelation},
		"zero created":                     {func(s *wager.State) { s.CreatedAt = time.Time{} }, wager.ErrInvalidTime},
		"updated before created":           {func(s *wager.State) { s.UpdatedAt = s.CreatedAt.Add(-time.Second) }, wager.ErrInvalidTime},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := processedState(t)
			c.mutate(&s)
			tx, err := wager.Rehydrate(s)
			require.ErrorIs(t, err, c.want)
			require.False(t, tx.IsValid())
		})
	}
}

// An internal opening has no provider, external id, idempotency key, hash, round,
// game or Reference. A stored one that has any of them is not an opening.
func TestAnOpeningCannotCarryExternalFields(t *testing.T) {
	fields := map[string]func(*wager.External){
		"provider":              func(e *wager.External) { e.ProviderID = "p" },
		"external id":           func(e *wager.External) { e.ExternalTransactionID = "e" },
		"idempotency key":       func(e *wager.External) { e.IdempotencyKey = "k" },
		"payload hash":          func(e *wager.External) { e.PayloadHash = "h" },
		"round":                 func(e *wager.External) { e.RoundID = "r" },
		"game":                  func(e *wager.External) { e.GameID = "g" },
		"external reference id": func(e *wager.External) { e.ReferenceExternalTransactionID = "x" },
		"resolved reference":    func(e *wager.External) { e.ReferenceTransactionID = uuid.New() },
	}
	for name, set := range fields {
		s := processedState(t)
		set(&s.External)
		_, err := wager.Rehydrate(s)
		require.ErrorIs(t, err, wager.ErrExternalFieldsOnOpening, name)
	}
}
