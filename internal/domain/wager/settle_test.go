package wager_test

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	domainwallet "github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// walletOf returns a wallet of the test player with the given balance in cents.
func walletOf(t *testing.T, minor int64) domainwallet.Wallet {
	t.Helper()
	state := domainwallet.State{
		ID: wallet, PlayerID: player, Balance: brl(t, minor), Version: 2,
		LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at,
	}
	w, err := domainwallet.Rehydrate(state)
	require.NoError(t, err)
	return w
}

func bet(t *testing.T, minor int64) wager.Transaction {
	t.Helper()
	p := betParams(t)
	p.Amount = brl(t, minor)
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k-" + uuid.NewString(), CorrelationID: "c", At: at})
	require.NoError(t, err)
	return tx
}

var settleAt = at.Add(time.Minute)

func TestABetDebitsTheWalletAndIsProcessedWithTheBalanceItLeft(t *testing.T) {
	tx, w := bet(t, 2500), walletOf(t, 10000)
	entryID := uuid.New()

	s, err := tx.Settle(w, entryID, settleAt)
	require.NoError(t, err)

	require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
	result, has := s.Transaction.Result()
	require.True(t, has)
	require.Equal(t, int64(7500), result.Balance.Minor())
	require.Equal(t, int64(3), result.WalletVersion, "the debit raised the version from 2")

	require.Equal(t, int64(7500), s.Wallet.Balance().Minor())
	require.Equal(t, int64(3), s.Wallet.Version())
	last, _ := s.Wallet.LastTransactionID()
	require.Equal(t, tx.ID(), last)

	require.True(t, s.Entry.IsValid())
	require.Equal(t, entryID, s.Entry.ID())
	require.Equal(t, domainwallet.DirectionDebit, s.Entry.Direction())
	require.Equal(t, tx.ID(), s.Entry.TransactionID())
	require.Equal(t, int64(2500), s.Entry.Amount().Minor())
	require.Equal(t, int64(10000), s.Entry.BalanceBefore().Minor())
	require.Equal(t, int64(7500), s.Entry.BalanceAfter().Minor())

	require.Equal(t, int64(10000), w.Balance().Minor(), "the wallet it was applied to is not changed")
	require.Equal(t, wager.StatusPending, tx.Status())
}

func TestABetForTheWholeBalanceLeavesZero(t *testing.T) {
	s, err := bet(t, 10000).Settle(walletOf(t, 10000), uuid.New(), settleAt)
	require.NoError(t, err)
	require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
	require.True(t, s.Wallet.Balance().IsZero())
}

func TestABetBeyondTheBalanceIsARejectionAndMovesNothing(t *testing.T) {
	for _, balance := range []int64{0, 7999, 79} {
		tx, w := bet(t, 8000), walletOf(t, balance)
		s, err := tx.Settle(w, uuid.New(), settleAt)
		require.NoError(t, err, "a rejection is an outcome, not an error")

		require.Equal(t, wager.StatusRejected, s.Transaction.Status())
		require.Equal(t, wager.FailureInsufficientFunds, s.Transaction.FailureCode())
		_, has := s.Transaction.Result()
		require.False(t, has)
		require.False(t, s.Entry.IsValid(), "a rejection writes no ledger entry")
		require.Equal(t, w, s.Wallet, "the wallet is exactly as it was: same balance, same version")
	}
}

func TestTheSecondOfTwoBetsThatDoNotBothFitIsRejected(t *testing.T) {
	// The scenario of the challenge: 100.00, two bets of 80.00.
	w := walletOf(t, 10000)
	first, err := bet(t, 8000).Settle(w, uuid.New(), settleAt)
	require.NoError(t, err)
	second, err := bet(t, 8000).Settle(first.Wallet, uuid.New(), settleAt)
	require.NoError(t, err)

	require.Equal(t, wager.StatusProcessed, first.Transaction.Status())
	require.Equal(t, wager.StatusRejected, second.Transaction.Status())
	require.Equal(t, wager.FailureInsufficientFunds, second.Transaction.FailureCode())
	require.Equal(t, int64(2000), second.Wallet.Balance().Minor())
	require.Equal(t, first.Wallet, second.Wallet)
}

func TestAnotherPlayersWalletIsRejectedBeforeAnythingElse(t *testing.T) {
	other := domainwallet.State{ID: wallet, PlayerID: uuid.New(), Balance: brl(t, 10000), Version: 2, LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at}
	w, err := domainwallet.Rehydrate(other)
	require.NoError(t, err)

	s, err := bet(t, 99999999).Settle(w, uuid.New(), settleAt)
	require.NoError(t, err)
	require.Equal(t, wager.StatusRejected, s.Transaction.Status())
	require.Equal(t, wager.FailurePlayerMismatch, s.Transaction.FailureCode(), "not INSUFFICIENT_FUNDS, though the bet is also too big")
	require.False(t, s.Entry.IsValid())
	require.Equal(t, w, s.Wallet)
}

func TestAWalletInAnotherCurrencyIsRejected(t *testing.T) {
	usd, err := money.FromMinor(10000, money.USD)
	require.NoError(t, err)
	w, err := domainwallet.Rehydrate(domainwallet.State{ID: wallet, PlayerID: player, Balance: usd, Version: 2, LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at})
	require.NoError(t, err)

	s, err := bet(t, 2500).Settle(w, uuid.New(), settleAt)
	require.NoError(t, err)
	require.Equal(t, wager.StatusRejected, s.Transaction.Status())
	require.Equal(t, wager.FailureCurrencyMismatch, s.Transaction.FailureCode())
	require.Equal(t, w, s.Wallet)
}

func TestSettlingIsRefusedWhenItCannotBeTheRightWalletOrTransaction(t *testing.T) {
	t.Run("another wallet than the transaction's", func(t *testing.T) {
		w, err := domainwallet.Rehydrate(domainwallet.State{ID: uuid.New(), PlayerID: player, Balance: brl(t, 10000), Version: 2, LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at})
		require.NoError(t, err)
		_, err = bet(t, 100).Settle(w, uuid.New(), settleAt)
		require.ErrorIs(t, err, wager.ErrWalletMismatch, "a caller error, not something to record as a rejection")
	})
	t.Run("an uninitialized wallet", func(t *testing.T) {
		_, err := bet(t, 100).Settle(domainwallet.Wallet{}, uuid.New(), settleAt)
		require.ErrorIs(t, err, wager.ErrUninitialized)
	})
	t.Run("an uninitialized transaction", func(t *testing.T) {
		_, err := wager.Transaction{}.Settle(walletOf(t, 100), uuid.New(), settleAt)
		require.ErrorIs(t, err, wager.ErrUninitialized)
	})
	t.Run("a transaction that already ended", func(t *testing.T) {
		done, err := bet(t, 100).Settle(walletOf(t, 10000), uuid.New(), settleAt)
		require.NoError(t, err)
		_, err = done.Transaction.Settle(done.Wallet, uuid.New(), settleAt.Add(time.Minute))
		require.ErrorIs(t, err, wager.ErrInvalidTransition, "never applied twice")
	})
	t.Run("no entry id", func(t *testing.T) {
		_, err := bet(t, 100).Settle(walletOf(t, 10000), uuid.Nil, settleAt)
		require.ErrorIs(t, err, domainwallet.ErrInvalidID)
	})
	t.Run("a time before the transaction was accepted", func(t *testing.T) {
		_, err := bet(t, 100).Settle(walletOf(t, 10000), uuid.New(), at.Add(-time.Second))
		require.Error(t, err)
	})
}

func TestAnOperationThatNamesAReferenceCannotBeSettledWithoutIt(t *testing.T) {
	for _, kind := range []wager.Kind{wager.KindRefund, wager.KindRollback} {
		p := betParams(t)
		p.Kind = kind
		p.ReferenceExternalTransactionID = "transaction-100"
		tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k", CorrelationID: "c", At: at})
		require.NoError(t, err)
		_, err = tx.Settle(walletOf(t, 10000), uuid.New(), settleAt)
		require.ErrorIs(t, err, wager.ErrReferenceUnresolved, kind)
	}
	win := winOf(t, 100, "transaction-100")
	_, err := win.Settle(walletOf(t, 10000), uuid.New(), settleAt)
	require.ErrorIs(t, err, wager.ErrReferenceUnresolved)
	_, err = bet(t, 100).SettleAgainst(wager.Reference{}, walletOf(t, 10000), uuid.New(), settleAt)
	require.ErrorIs(t, err, wager.ErrNoReference, "and an operation that names none has nothing to settle against")
	_, err = pendingOpening(t).Settle(walletOf(t, 10000), uuid.New(), settleAt)
	require.ErrorIs(t, err, wager.ErrKindNotSupported)
}

func operationOf(t *testing.T, kind wager.Kind, minor int64) wager.Transaction {
	t.Helper()
	p := betParams(t)
	p.Kind = kind
	p.Amount = brl(t, minor)
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k-" + uuid.NewString(), CorrelationID: "c", At: at})
	require.NoError(t, err)
	return tx
}

func TestAWinCreditsTheWalletAndIsProcessedWithTheBalanceItLeft(t *testing.T) {
	tx, w := operationOf(t, wager.KindWin, 4000), walletOf(t, 10000)
	entryID := uuid.New()

	s, err := tx.Settle(w, entryID, settleAt)
	require.NoError(t, err)

	require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
	result, _ := s.Transaction.Result()
	require.Equal(t, int64(14000), result.Balance.Minor())
	require.Equal(t, int64(3), result.WalletVersion)
	require.Equal(t, int64(14000), s.Wallet.Balance().Minor())
	require.Equal(t, int64(3), s.Wallet.Version())
	last, _ := s.Wallet.LastTransactionID()
	require.Equal(t, tx.ID(), last)

	require.Equal(t, entryID, s.Entry.ID())
	require.Equal(t, domainwallet.DirectionCredit, s.Entry.Direction())
	require.Equal(t, int64(4000), s.Entry.Amount().Minor())
	require.Equal(t, int64(10000), s.Entry.BalanceBefore().Minor())
	require.Equal(t, int64(14000), s.Entry.BalanceAfter().Minor())
	require.Equal(t, int64(10000), w.Balance().Minor(), "the wallet it was applied to is not changed")
}

func TestAWinIntoAnEmptyWalletWorks(t *testing.T) {
	s, err := operationOf(t, wager.KindWin, 1).Settle(walletOf(t, 0), uuid.New(), settleAt)
	require.NoError(t, err)
	require.Equal(t, int64(1), s.Wallet.Balance().Minor())
}

func TestAWinThatTheBalanceCouldNotHoldIsARejectionAndMovesNothing(t *testing.T) {
	tx, w := operationOf(t, wager.KindWin, 1), walletOf(t, math.MaxInt64)

	s, err := tx.Settle(w, uuid.New(), settleAt)

	require.NoError(t, err, "a limit of the data is a Rejection, not an error of the call")
	require.Equal(t, wager.StatusRejected, s.Transaction.Status())
	require.Equal(t, wager.FailureBalanceLimitExceeded, s.Transaction.FailureCode())
	require.False(t, s.Entry.IsValid())
	require.Equal(t, w, s.Wallet)

	s, err = operationOf(t, wager.KindWin, 1).Settle(walletOf(t, math.MaxInt64-1), uuid.New(), settleAt)
	require.NoError(t, err)
	require.Equal(t, wager.StatusProcessed, s.Transaction.Status(), "one below the limit still fits")
}

func TestALossMovesNothingAndTheWalletKeepsItsVersion(t *testing.T) {
	tx, w := operationOf(t, wager.KindLoss, 0), walletOf(t, 10000)

	s, err := tx.Settle(w, uuid.New(), settleAt)
	require.NoError(t, err)

	require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
	result, _ := s.Transaction.Result()
	require.Equal(t, int64(10000), result.Balance.Minor(), "it reports the balance as it stands")
	require.Equal(t, int64(2), result.WalletVersion, "and the version it stands at")
	require.False(t, s.Entry.IsValid(), "no Ledger Entry")
	require.Equal(t, w, s.Wallet, "the wallet is the very same")
	require.Equal(t, int64(2), s.Wallet.Version())
	before, _ := w.LastTransactionID()
	after, _ := s.Wallet.LastTransactionID()
	require.Equal(t, before, after)
}

func TestALossIsCheckedAgainstTheWalletToo(t *testing.T) {
	s, err := operationOf(t, wager.KindLoss, 0).Settle(walletOf(t, 0), uuid.New(), settleAt)
	require.NoError(t, err)
	require.Equal(t, wager.StatusProcessed, s.Transaction.Status(), "an empty wallet can lose")
}

func TestWinsAndLossesAreRefusedForTheWrongPlayerOrCurrency(t *testing.T) {
	for _, kind := range []wager.Kind{wager.KindWin, wager.KindLoss} {
		minor := int64(100)
		if kind == wager.KindLoss {
			minor = 0
		}
		other := domainwallet.State{ID: wallet, PlayerID: uuid.New(), Balance: brl(t, 500), Version: 2, LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at}
		stranger, err := domainwallet.Rehydrate(other)
		require.NoError(t, err)
		s, err := operationOf(t, kind, minor).Settle(stranger, uuid.New(), settleAt)
		require.NoError(t, err, kind)
		require.Equal(t, wager.FailurePlayerMismatch, s.Transaction.FailureCode(), kind)
		require.False(t, s.Entry.IsValid(), kind)
		require.Equal(t, stranger, s.Wallet, kind)

		usd, err := money.FromMinor(500, money.USD)
		require.NoError(t, err)
		abroad, err := domainwallet.Rehydrate(domainwallet.State{ID: wallet, PlayerID: player, Balance: usd, Version: 2, LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at})
		require.NoError(t, err)
		s, err = operationOf(t, kind, minor).Settle(abroad, uuid.New(), settleAt)
		require.NoError(t, err, kind)
		require.Equal(t, wager.FailureCurrencyMismatch, s.Transaction.FailureCode(), kind)
		require.False(t, s.Entry.IsValid(), kind)
	}
}

func TestAnOverflowingDebitIsAnErrorNotARejection(t *testing.T) {
	// Debits cannot overflow a non-negative balance, so this is only a guard: the
	// largest bet against the largest balance still works.
	s, err := bet(t, math.MaxInt64).Settle(walletOf(t, math.MaxInt64), uuid.New(), settleAt)
	require.NoError(t, err)
	require.True(t, s.Wallet.Balance().IsZero())
}

// Whatever the sequence of bets, the balance never goes negative, the accepted
// ones are exactly the ones that fit, and the ledger entries chain.
func TestRandomSequencesOfBetsKeepTheInvariants(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*104729)) //nolint:gosec // a seeded generator keeps the sequences reproducible
		w := walletOf(t, int64(rng.IntN(200_000)))
		expected := w.Balance().Minor()
		var previousAfter *int64
		accepted := int64(0)

		for i := 0; i < 150; i++ {
			amount := int64(rng.IntN(70_000) + 1)
			s, err := bet(t, amount).Settle(w, uuid.New(), settleAt)
			require.NoError(t, err)

			if amount <= expected {
				require.Equal(t, wager.StatusProcessed, s.Transaction.Status(), "seed %d step %d: it fits", seed, i)
				expected -= amount
				accepted++
				require.Equal(t, w.Balance().Minor(), s.Entry.BalanceBefore().Minor())
				if previousAfter != nil {
					require.Equal(t, *previousAfter, s.Entry.BalanceBefore().Minor(), "the entries chain")
				}
				after := s.Entry.BalanceAfter().Minor()
				previousAfter = &after
			} else {
				require.Equal(t, wager.StatusRejected, s.Transaction.Status(), "seed %d step %d: it does not fit", seed, i)
				require.Equal(t, wager.FailureInsufficientFunds, s.Transaction.FailureCode())
				require.False(t, s.Entry.IsValid())
			}
			w = s.Wallet
			require.Equal(t, expected, w.Balance().Minor())
			require.GreaterOrEqual(t, w.Balance().Minor(), int64(0))
			require.Equal(t, 2+accepted, w.Version(), "the version rises once per accepted bet and only then")
		}
	}
}
