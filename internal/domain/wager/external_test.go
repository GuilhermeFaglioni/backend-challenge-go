package wager_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

func acceptedBet(t *testing.T) wager.Transaction {
	t.Helper()
	tx, err := wager.New(wager.NewParams{
		ID: uuid.New(), Request: newRequest(t, betParams(t)), IdempotencyKey: "provider-a:transaction-123",
		CorrelationID: "corr-1", At: at,
	})
	require.NoError(t, err)
	return tx
}

func acceptedRefund(t *testing.T) wager.Transaction {
	t.Helper()
	tx, err := wager.New(wager.NewParams{
		ID: uuid.New(), Request: newRequest(t, refundParams(t)), IdempotencyKey: "provider-a:transaction-123",
		CorrelationID: "corr-1", At: at,
	})
	require.NoError(t, err)
	return tx
}

func TestAnExternalRequestIsAcceptedPendingWithItsHashAndKey(t *testing.T) {
	request := newRequest(t, refundParams(t))
	id := uuid.New()
	tx, err := wager.New(wager.NewParams{ID: id, Request: request, IdempotencyKey: "key-as-received", CorrelationID: "corr-1", At: at})
	require.NoError(t, err)

	require.True(t, tx.IsValid())
	require.Equal(t, id, tx.ID())
	require.Equal(t, wager.KindRefund, tx.Kind())
	require.Equal(t, wager.OriginExternal, tx.Origin())
	require.Equal(t, wager.StatusPending, tx.Status())
	require.Equal(t, wallet, tx.WalletID())
	require.Equal(t, wallet, tx.RequestedWalletID())
	require.Equal(t, player, tx.PlayerID())
	require.Equal(t, request.Amount(), tx.Amount())
	require.Equal(t, "corr-1", tx.CorrelationID())
	require.Equal(t, at, tx.CreatedAt())
	require.Equal(t, at, tx.UpdatedAt())

	external := tx.External()
	require.Equal(t, "provider-a", external.ProviderID)
	require.Equal(t, "transaction-123", external.ExternalTransactionID)
	require.Equal(t, "key-as-received", external.IdempotencyKey, "the key is kept exactly as it came, never replaced")
	require.Equal(t, request.Hash(), external.PayloadHash)
	require.Equal(t, "round-987", external.RoundID)
	require.Equal(t, "fortune-chimp", external.GameID)
	require.Equal(t, "transaction-100", external.ReferenceExternalTransactionID)
	require.Equal(t, uuid.Nil, external.ReferenceTransactionID, "not resolved yet")
}

// The idempotency key is transport metadata: whatever it is, the hash is the same.
func TestTheHashDoesNotDependOnTheIdempotencyKey(t *testing.T) {
	request := newRequest(t, betParams(t))
	first, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request, IdempotencyKey: "key-one", CorrelationID: "c", At: at})
	require.NoError(t, err)
	second, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request, IdempotencyKey: "a-completely-different-key", CorrelationID: "c", At: at.Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, first.External().PayloadHash, second.External().PayloadHash)
}

func TestAcceptingARequestRefusesWhatItCannotWorkWith(t *testing.T) {
	good := wager.NewParams{ID: uuid.New(), Request: newRequest(t, betParams(t)), IdempotencyKey: "key", CorrelationID: "c", At: at}
	cases := map[string]struct {
		mutate func(*wager.NewParams)
		want   error
	}{
		"nil id":           {func(p *wager.NewParams) { p.ID = uuid.Nil }, wager.ErrInvalidID},
		"no request":       {func(p *wager.NewParams) { p.Request = wager.Request{} }, wager.ErrUninitialized},
		"no key":           {func(p *wager.NewParams) { p.IdempotencyKey = "" }, wager.ErrInvalidIdempotencyKey},
		"key with a space": {func(p *wager.NewParams) { p.IdempotencyKey = "two words" }, wager.ErrInvalidIdempotencyKey},
		"key with control": {func(p *wager.NewParams) { p.IdempotencyKey = "a\tb" }, wager.ErrInvalidIdempotencyKey},
		"key not ASCII":    {func(p *wager.NewParams) { p.IdempotencyKey = "chave-ção" }, wager.ErrInvalidIdempotencyKey},
		"key too long":     {func(p *wager.NewParams) { p.IdempotencyKey = strings.Repeat("k", 256) }, wager.ErrInvalidIdempotencyKey},
		"no correlation":   {func(p *wager.NewParams) { p.CorrelationID = "" }, wager.ErrInvalidCorrelation},
		"zero time":        {func(p *wager.NewParams) { p.At = time.Time{} }, wager.ErrInvalidTime},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := good
			c.mutate(&p)
			tx, err := wager.New(p)
			require.ErrorIs(t, err, c.want)
			require.False(t, tx.IsValid())
		})
	}
	p := good
	p.IdempotencyKey = strings.Repeat("k", 255)
	_, err := wager.New(p)
	require.NoError(t, err, "255 characters is the limit and is allowed")
}

// --- the transitions ---------------------------------------------------------------

func TestAnExternalTransactionIsProcessedWithTheWalletItLeft(t *testing.T) {
	tx := acceptedBet(t)
	done, err := tx.Process(wager.Result{Balance: brl(t, 7500), WalletVersion: 4}, at.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, wager.StatusProcessed, done.Status())
	result, has := done.Result()
	require.True(t, has)
	require.Equal(t, int64(7500), result.Balance.Minor())
	require.Equal(t, int64(4), result.WalletVersion)
	_, has = done.ProcessedAt()
	require.True(t, has)
	require.Equal(t, wager.StatusPending, tx.Status())
}

func TestAnExternalResultMustBeAUsableBalance(t *testing.T) {
	tx := acceptedBet(t)
	usd, err := money.FromMinor(100, money.USD)
	require.NoError(t, err)
	for name, result := range map[string]wager.Result{
		"negative balance": {Balance: mustBRL(-1), WalletVersion: 2},
		"no balance":       {WalletVersion: 2},
		"other currency":   {Balance: usd, WalletVersion: 2},
		"version zero":     {Balance: brl(t, 0), WalletVersion: 0},
	} {
		_, err := tx.Process(result, at.Add(time.Second))
		require.ErrorIs(t, err, wager.ErrInvalidResult, name)
	}
	_, err = tx.Process(wager.Result{Balance: brl(t, 0), WalletVersion: 1}, at.Add(time.Second))
	require.NoError(t, err, "an emptied wallet at version 1 (a LOSS leaves the version alone) is fine")
}

func TestARejectionRecordsItsCode(t *testing.T) {
	for _, code := range wager.FailureCodes() {
		if !code.IsRejection() {
			continue
		}
		rejected, err := acceptedBet(t).Reject(code, at.Add(time.Second))
		require.NoError(t, err, code)
		require.Equal(t, wager.StatusRejected, rejected.Status())
		require.Equal(t, code, rejected.FailureCode())
		_, has := rejected.Result()
		require.False(t, has, "a rejection moved no money")
		_, has = rejected.ProcessedAt()
		require.True(t, has)
	}
}

func TestOnlyARejectionCodeRejects(t *testing.T) {
	for _, code := range []wager.FailureCode{wager.FailureInternalPermanent, "", "MADE_UP", "insufficient_funds"} {
		_, err := acceptedBet(t).Reject(code, at.Add(time.Second))
		require.ErrorIs(t, err, wager.ErrInvalidFailureCode, "%q", code)
	}
}

func TestAnUnknownWalletRejectionKeepsOnlyTheWalletThatWasAskedFor(t *testing.T) {
	rejected, err := acceptedBet(t).Reject(wager.FailureWalletNotFound, at.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, uuid.Nil, rejected.WalletID(), "there is no such wallet")
	require.Equal(t, wallet, rejected.RequestedWalletID(), "but the id that arrived is not lost")

	other, err := acceptedBet(t).Reject(wager.FailureInsufficientFunds, at.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, wallet, other.WalletID(), "every other rejection has its wallet")
}

func TestAnOpeningIsNeverRejected(t *testing.T) {
	_, err := pendingOpening(t).Reject(wager.FailureInsufficientFunds, at.Add(time.Second))
	require.ErrorIs(t, err, wager.ErrInvalidStatus)
}

func TestWaitingForAReferenceNeedsAReference(t *testing.T) {
	waiting, err := acceptedRefund(t).AwaitReference(at.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, wager.StatusPendingReference, waiting.Status())
	_, has := waiting.ProcessedAt()
	require.False(t, has, "waiting is not an ending")

	_, err = acceptedBet(t).AwaitReference(at.Add(time.Second))
	require.ErrorIs(t, err, wager.ErrNoReference, "a BET has nothing to wait for")
	_, err = pendingOpening(t).AwaitReference(at.Add(time.Second))
	require.ErrorIs(t, err, wager.ErrNoReference)
}

func TestAWinWithAReferenceCanWaitAndWithoutOneCannot(t *testing.T) {
	with := betParams(t)
	with.Kind, with.ReferenceExternalTransactionID = wager.KindWin, "transaction-100"
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, with), IdempotencyKey: "k", CorrelationID: "c", At: at})
	require.NoError(t, err)
	_, err = tx.AwaitReference(at.Add(time.Second))
	require.NoError(t, err)

	without := betParams(t)
	without.Kind = wager.KindWin
	tx, err = wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, without), IdempotencyKey: "k", CorrelationID: "c", At: at})
	require.NoError(t, err)
	_, err = tx.AwaitReference(at.Add(time.Second))
	require.ErrorIs(t, err, wager.ErrNoReference)
}

func TestAWaitingTransactionCanStillEndInAnyWay(t *testing.T) {
	waiting, err := acceptedRefund(t).AwaitReference(at.Add(time.Second))
	require.NoError(t, err)
	later := at.Add(time.Minute)

	processed, err := waiting.Process(wager.Result{Balance: brl(t, 5000), WalletVersion: 3}, later)
	require.NoError(t, err)
	require.Equal(t, wager.StatusProcessed, processed.Status())
	rejected, err := waiting.Reject(wager.FailureReferenceNotFound, later)
	require.NoError(t, err)
	require.Equal(t, wager.StatusRejected, rejected.Status())
	failed, err := waiting.Fail(later)
	require.NoError(t, err)
	require.Equal(t, wager.StatusFailed, failed.Status())

	_, err = waiting.AwaitReference(later)
	require.ErrorIs(t, err, wager.ErrInvalidTransition, "waiting again is not a transition: the worker just tries again")
}

func TestEveryEndingIsFinalForEveryMethod(t *testing.T) {
	tx := acceptedRefund(t)
	later := at.Add(time.Second)
	processed, err := tx.Process(wager.Result{Balance: brl(t, 5000), WalletVersion: 3}, later)
	require.NoError(t, err)
	rejected, err := tx.Reject(wager.FailureInsufficientFunds, later)
	require.NoError(t, err)
	failed, err := tx.Fail(later)
	require.NoError(t, err)

	for name, terminal := range map[string]wager.Transaction{"processed": processed, "rejected": rejected, "failed": failed} {
		_, err := terminal.Process(wager.Result{Balance: brl(t, 1), WalletVersion: 1}, later.Add(time.Minute))
		require.ErrorIs(t, err, wager.ErrInvalidTransition, name+" processed")
		_, err = terminal.Reject(wager.FailureInsufficientFunds, later.Add(time.Minute))
		require.ErrorIs(t, err, wager.ErrInvalidTransition, name+" rejected")
		_, err = terminal.AwaitReference(later.Add(time.Minute))
		require.ErrorIs(t, err, wager.ErrInvalidTransition, name+" waiting")
		_, err = terminal.Fail(later.Add(time.Minute))
		require.ErrorIs(t, err, wager.ErrInvalidTransition, name+" failed")
	}
}

// --- rehydration -------------------------------------------------------------------

func storedBet(t *testing.T) wager.State {
	t.Helper()
	request := newRequest(t, betParams(t))
	processed := at.Add(time.Second)
	return wager.State{
		ID: uuid.New(), Kind: wager.KindBet, RequestedWalletID: wallet, WalletID: wallet, PlayerID: player,
		Amount: request.Amount(), Status: wager.StatusProcessed,
		Result: &wager.Result{Balance: brl(t, 7500), WalletVersion: 3},
		External: wager.External{
			ProviderID: "provider-a", ExternalTransactionID: "transaction-123", IdempotencyKey: "provider-a:transaction-123",
			PayloadHash: request.Hash(), RoundID: "round-987", GameID: "fortune-chimp",
		},
		CorrelationID: "corr-1", CreatedAt: at, UpdatedAt: processed, ProcessedAt: &processed,
	}
}

func storedRefundWaiting(t *testing.T) wager.State {
	t.Helper()
	request := newRequest(t, refundParams(t))
	s := storedBet(t)
	s.Kind, s.Status, s.Result, s.ProcessedAt, s.UpdatedAt = wager.KindRefund, wager.StatusPendingReference, nil, nil, at
	s.External.PayloadHash = request.Hash()
	s.External.ReferenceExternalTransactionID = "transaction-100"
	return s
}

func TestRehydratingEveryStatusOfAnExternalTransaction(t *testing.T) {
	processedAt := at.Add(time.Second)
	cases := map[string]func() wager.State{
		"processed": func() wager.State { return storedBet(t) },
		"pending": func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.ProcessedAt, s.UpdatedAt = wager.StatusPending, nil, nil, at
			return s
		},
		"waiting for a reference": func() wager.State { return storedRefundWaiting(t) },
		"rejected": func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.FailureCode = wager.StatusRejected, nil, wager.FailureInsufficientFunds
			return s
		},
		"rejected, wallet not found": func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.FailureCode, s.WalletID = wager.StatusRejected, nil, wager.FailureWalletNotFound, uuid.Nil
			return s
		},
		"failed": func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.FailureCode = wager.StatusFailed, nil, wager.FailureInternalPermanent
			return s
		},
		"a refund whose reference was resolved": func() wager.State {
			s := storedRefundWaiting(t)
			s.Status, s.Result, s.ProcessedAt, s.UpdatedAt = wager.StatusProcessed, &wager.Result{Balance: brl(t, 5000), WalletVersion: 3}, &processedAt, processedAt
			s.External.ReferenceTransactionID = uuid.New()
			return s
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			s := build()
			tx, err := wager.Rehydrate(s)
			require.NoError(t, err)
			require.Equal(t, s.Status, tx.Status())
			require.Equal(t, s.External, tx.External())
			require.Equal(t, s.WalletID, tx.WalletID())
			require.Equal(t, s.RequestedWalletID, tx.RequestedWalletID())
		})
	}
}

func TestRehydratingRefusesWhatCouldNotHaveBeenStored(t *testing.T) {
	cases := map[string]struct {
		build func() wager.State
		want  error
	}{
		"the hash is not the request's": {func() wager.State {
			s := storedBet(t)
			s.External.PayloadHash = wager.PayloadHash(strings.Repeat("0", 64))
			return s
		}, wager.ErrPayloadHashMismatch},
		"the amount was edited": {func() wager.State { s := storedBet(t); s.Amount = brl(t, 2501); return s }, wager.ErrPayloadHashMismatch},
		"the round was edited":  {func() wager.State { s := storedBet(t); s.External.RoundID = "round-1"; return s }, wager.ErrPayloadHashMismatch},
		"no key":                {func() wager.State { s := storedBet(t); s.External.IdempotencyKey = ""; return s }, wager.ErrInvalidIdempotencyKey},
		"no provider":           {func() wager.State { s := storedBet(t); s.External.ProviderID = ""; return s }, wager.ErrInvalidProvider},
		"no correlation":        {func() wager.State { s := storedBet(t); s.CorrelationID = ""; return s }, wager.ErrInvalidCorrelation},
		"nil id":                {func() wager.State { s := storedBet(t); s.ID = uuid.Nil; return s }, wager.ErrInvalidID},
		"a resolved reference without a reference": {func() wager.State {
			s := storedBet(t)
			s.External.ReferenceTransactionID = uuid.New()
			return s
		}, wager.ErrInvalidID},
		"a wallet other than the one asked for":   {func() wager.State { s := storedBet(t); s.WalletID = uuid.New(); return s }, wager.ErrInvalidID},
		"no wallet but not a not-found rejection": {func() wager.State { s := storedBet(t); s.WalletID = uuid.Nil; return s }, wager.ErrInvalidID},
		"a not-found rejection that has a wallet": {func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.FailureCode = wager.StatusRejected, nil, wager.FailureWalletNotFound
			return s
		}, wager.ErrInvalidID},
		"a BET that waits for a reference": {func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.ProcessedAt, s.UpdatedAt = wager.StatusPendingReference, nil, nil, at
			return s
		}, wager.ErrInvalidStatus},
		"pending with a result": {func() wager.State {
			s := storedBet(t)
			s.Status, s.ProcessedAt, s.UpdatedAt = wager.StatusPending, nil, at
			return s
		}, wager.ErrInvalidResult},
		"processed without a result":    {func() wager.State { s := storedBet(t); s.Result = nil; return s }, wager.ErrInvalidResult},
		"processed with a failure code": {func() wager.State { s := storedBet(t); s.FailureCode = wager.FailureInsufficientFunds; return s }, wager.ErrInvalidResult},
		"processed with a negative balance": {func() wager.State {
			s := storedBet(t)
			s.Result = &wager.Result{Balance: mustBRL(-5), WalletVersion: 2}
			return s
		}, wager.ErrInvalidResult},
		"rejected without a code": {func() wager.State {
			s := storedBet(t)
			s.Status, s.Result = wager.StatusRejected, nil
			return s
		}, wager.ErrInvalidResult},
		"rejected with the failure code": {func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.FailureCode = wager.StatusRejected, nil, wager.FailureInternalPermanent
			return s
		}, wager.ErrInvalidResult},
		"rejected with a result": {func() wager.State {
			s := storedBet(t)
			s.Status, s.FailureCode = wager.StatusRejected, wager.FailureInsufficientFunds
			return s
		}, wager.ErrInvalidResult},
		"failed with another code": {func() wager.State {
			s := storedBet(t)
			s.Status, s.Result, s.FailureCode = wager.StatusFailed, nil, wager.FailureInsufficientFunds
			return s
		}, wager.ErrInvalidResult},
		"a finished transaction without its time": {func() wager.State { s := storedBet(t); s.ProcessedAt = nil; return s }, wager.ErrInvalidTime},
		"an unfinished one with a time": {func() wager.State {
			s := storedRefundWaiting(t)
			processed := at
			s.ProcessedAt = &processed
			return s
		}, wager.ErrInvalidTime},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tx, err := wager.Rehydrate(c.build())
			require.ErrorIs(t, err, c.want)
			require.False(t, tx.IsValid())
		})
	}
}

func TestAnOpeningNeedsItsOwnWalletAsTheRequestedOne(t *testing.T) {
	s := processedState(t)
	s.RequestedWalletID = uuid.New()
	_, err := wager.Rehydrate(s)
	require.ErrorIs(t, err, wager.ErrInvalidID)
}
