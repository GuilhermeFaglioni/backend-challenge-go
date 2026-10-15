package wager_test

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	domainwallet "github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// processed is a transaction that went through: it is what an operation can name.
func processed(t *testing.T, kind wager.Kind, minor int64) wager.Transaction {
	t.Helper()
	p := betParams(t)
	p.Kind, p.Amount = kind, brl(t, minor)
	if kind == wager.KindRefund || kind == wager.KindRollback {
		p.ReferenceExternalTransactionID = "transaction-0"
	}
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "ref-" + uuid.NewString(), CorrelationID: "c", At: at})
	require.NoError(t, err)
	done, err := tx.Process(wager.Result{Balance: brl(t, 1), WalletVersion: 2}, at)
	require.NoError(t, err)
	return done
}

func ended(t *testing.T, status wager.Status) wager.Transaction {
	t.Helper()
	tx := bet(t, 2500)
	var err error
	switch status {
	case wager.StatusRejected:
		tx, err = tx.Reject(wager.FailureInsufficientFunds, settleAt)
	case wager.StatusFailed:
		tx, err = tx.Fail(settleAt)
	case wager.StatusPending:
	}
	require.NoError(t, err)
	return tx
}

func reversal(t *testing.T, kind wager.Kind, minor int64) wager.Transaction {
	t.Helper()
	p := betParams(t)
	p.Kind, p.Amount, p.ReferenceExternalTransactionID = kind, brl(t, minor), "transaction-100"
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k-" + uuid.NewString(), CorrelationID: "c", At: at})
	require.NoError(t, err)
	return tx
}

func winOf(t *testing.T, minor int64, reference string) wager.Transaction {
	t.Helper()
	p := betParams(t)
	p.Kind, p.Amount, p.ReferenceExternalTransactionID = wager.KindWin, brl(t, minor), reference
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k-" + uuid.NewString(), CorrelationID: "c", At: at})
	require.NoError(t, err)
	return tx
}

func found(tx wager.Transaction) wager.Reference {
	return wager.Reference{Found: true, Transaction: tx}
}

func settleAgainst(t *testing.T, tx wager.Transaction, ref wager.Reference, balance int64) wager.Settlement {
	t.Helper()
	s, err := tx.SettleAgainst(ref, walletOf(t, balance), uuid.New(), settleAt, false)
	require.NoError(t, err)
	return s
}

func requireRejected(t *testing.T, s wager.Settlement, code wager.FailureCode, msg string) {
	t.Helper()
	require.Equal(t, wager.StatusRejected, s.Transaction.Status(), msg)
	require.Equal(t, code, s.Transaction.FailureCode(), msg)
	require.False(t, s.Entry.IsValid(), msg)
	require.Equal(t, int64(10000), s.Wallet.Balance().Minor(), msg)
	require.Equal(t, int64(2), s.Wallet.Version(), msg)
}

// --- the movement of each reversal -------------------------------------------------------

func TestARefundCreditsWhatTheBetTook(t *testing.T) {
	ref := processed(t, wager.KindBet, 2500)
	tx := reversal(t, wager.KindRefund, 2500)

	s := settleAgainst(t, tx, found(ref), 10000)

	require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
	require.Equal(t, int64(12500), s.Wallet.Balance().Minor())
	require.Equal(t, domainwallet.DirectionCredit, s.Entry.Direction())
	require.Equal(t, int64(2500), s.Entry.Amount().Minor())
	require.Equal(t, ref.ID(), s.Transaction.External().ReferenceTransactionID, "the resolved Reference is kept")
	result, _ := s.Transaction.Result()
	require.Equal(t, int64(12500), result.Balance.Minor())
	require.Equal(t, int64(3), result.WalletVersion)
}

func TestARollbackDoesTheContraryOfWhatItUndoes(t *testing.T) {
	cases := map[string]struct {
		reference wager.Kind
		direction domainwallet.Direction
		balance   int64
	}{
		"of a bet it gives the money back":     {wager.KindBet, domainwallet.DirectionCredit, 12500},
		"of a win it takes the prize back":     {wager.KindWin, domainwallet.DirectionDebit, 7500},
		"of a refund it takes the refund back": {wager.KindRefund, domainwallet.DirectionDebit, 7500},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ref := processed(t, c.reference, 2500)
			s := settleAgainst(t, reversal(t, wager.KindRollback, 2500), found(ref), 10000)

			require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
			require.Equal(t, c.direction, s.Entry.Direction())
			require.Equal(t, c.balance, s.Wallet.Balance().Minor())
			require.Equal(t, ref.ID(), s.Transaction.External().ReferenceTransactionID)
		})
	}
}

// --- every rule of the table of References, each with its own code ------------------------

func TestEachRuleOfTheTableOfReferencesHasItsOwnCode(t *testing.T) {
	bet2500 := processed(t, wager.KindBet, 2500)

	t.Run("not found", func(t *testing.T) {
		s := settleAgainst(t, reversal(t, wager.KindRefund, 2500), wager.Reference{}, 10000)
		requireRejected(t, s, wager.FailureReferenceNotFound, "")
		require.Equal(t, uuid.Nil, s.Transaction.External().ReferenceTransactionID, "there is nothing to keep")
	})
	t.Run("found but never completed, once it may not wait any more", func(t *testing.T) {
		s := settleAgainst(t, reversal(t, wager.KindRefund, 2500), found(ended(t, wager.StatusPending)), 10000)
		requireRejected(t, s, wager.FailureReferenceNotProcessed, "")
	})
	t.Run("rejected or failed is not processed", func(t *testing.T) {
		for _, status := range []wager.Status{wager.StatusRejected, wager.StatusFailed} {
			s := settleAgainst(t, reversal(t, wager.KindRefund, 2500), found(ended(t, status)), 10000)
			requireRejected(t, s, wager.FailureReferenceNotProcessed, string(status))
		}
	})
	t.Run("kinds this cannot act on", func(t *testing.T) {
		for name, c := range map[string]struct {
			op        wager.Kind
			reference wager.Kind
		}{
			"a refund of a win":        {wager.KindRefund, wager.KindWin},
			"a refund of a loss":       {wager.KindRefund, wager.KindLoss},
			"a refund of a refund":     {wager.KindRefund, wager.KindRefund},
			"a refund of a rollback":   {wager.KindRefund, wager.KindRollback},
			"a rollback of a rollback": {wager.KindRollback, wager.KindRollback},
			"a rollback of a loss":     {wager.KindRollback, wager.KindLoss},
			"a win naming a win":       {wager.KindWin, wager.KindWin},
			"a win naming a loss":      {wager.KindWin, wager.KindLoss},
			"a win naming a refund":    {wager.KindWin, wager.KindRefund},
		} {
			amount := int64(2500)
			var tx wager.Transaction
			if c.op == wager.KindWin {
				tx = winOf(t, amount, "transaction-100")
			} else {
				tx = reversal(t, c.op, amount)
			}
			refAmount := amount
			if c.reference == wager.KindLoss {
				refAmount = 0
			}
			s := settleAgainst(t, tx, found(processed(t, c.reference, refAmount)), 10000)
			requireRejected(t, s, wager.FailureReferenceKindInvalid, name)
		}
	})
	t.Run("another player, wallet, currency or round", func(t *testing.T) {
		mismatch := func(change func(*wager.RequestParams)) wager.Transaction {
			p := betParams(t)
			change(&p)
			tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k", CorrelationID: "c", At: at})
			require.NoError(t, err)
			done, err := tx.Process(wager.Result{Balance: p.Amount, WalletVersion: 2}, at)
			require.NoError(t, err)
			return done
		}
		for name, ref := range map[string]wager.Transaction{
			"player":   mismatch(func(p *wager.RequestParams) { p.PlayerID = uuid.New() }),
			"wallet":   mismatch(func(p *wager.RequestParams) { p.WalletID = uuid.New() }),
			"round":    mismatch(func(p *wager.RequestParams) { p.RoundID = "another-round" }),
			"currency": mismatch(func(p *wager.RequestParams) { usd, _ := money.FromMinor(2500, money.USD); p.Amount = usd }),
		} {
			s := settleAgainst(t, reversal(t, wager.KindRefund, 2500), found(ref), 10000)
			requireRejected(t, s, wager.FailureReferenceMismatch, name)
		}
	})
	t.Run("another amount", func(t *testing.T) {
		for _, amount := range []int64{2499, 2501, 1} {
			s := settleAgainst(t, reversal(t, wager.KindRefund, amount), found(bet2500), 10000)
			requireRejected(t, s, wager.FailureReferenceAmountMismatch, "")
		}
	})
	t.Run("already reversed", func(t *testing.T) {
		ref := wager.Reference{Found: true, Transaction: bet2500, Reversed: true}
		for _, kind := range []wager.Kind{wager.KindRefund, wager.KindRollback} {
			s := settleAgainst(t, reversal(t, kind, 2500), ref, 10000)
			requireRejected(t, s, wager.FailureReferenceAlreadyReversed, string(kind))
		}
	})
	t.Run("a reversal that would overdraw", func(t *testing.T) {
		win := processed(t, wager.KindWin, 2500)
		s, err := reversal(t, wager.KindRollback, 2500).SettleAgainst(found(win), walletOf(t, 2499), uuid.New(), settleAt, false)
		require.NoError(t, err)
		require.Equal(t, wager.FailureReversalInsufficientFunds, s.Transaction.FailureCode(), "told apart from a bet's INSUFFICIENT_FUNDS")
		require.Equal(t, int64(2499), s.Wallet.Balance().Minor())
		require.False(t, s.Entry.IsValid())

		s, err = reversal(t, wager.KindRollback, 2500).SettleAgainst(found(win), walletOf(t, 2500), uuid.New(), settleAt, false)
		require.NoError(t, err)
		require.Equal(t, wager.StatusProcessed, s.Transaction.Status(), "exactly the balance still works")
		require.True(t, s.Wallet.Balance().IsZero())
	})
}

func TestTheRulesAreCheckedInTheOrderOfTheTable(t *testing.T) {
	// A refund of a win in another round with another amount: the kind comes first.
	win := processed(t, wager.KindWin, 999)
	s := settleAgainst(t, reversal(t, wager.KindRefund, 2500), found(win), 10000)
	require.Equal(t, wager.FailureReferenceKindInvalid, s.Transaction.FailureCode())

	// A bet of another player with another amount, already reversed: the mismatch first.
	p := betParams(t)
	p.PlayerID = uuid.New()
	other, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k", CorrelationID: "c", At: at})
	require.NoError(t, err)
	other, err = other.Process(wager.Result{Balance: brl(t, 1), WalletVersion: 2}, at)
	require.NoError(t, err)
	s = settleAgainst(t, reversal(t, wager.KindRefund, 1), wager.Reference{Found: true, Transaction: other, Reversed: true}, 10000)
	require.Equal(t, wager.FailureReferenceMismatch, s.Transaction.FailureCode())

	// The amount comes before "already reversed".
	s = settleAgainst(t, reversal(t, wager.KindRefund, 1), wager.Reference{Found: true, Transaction: processed(t, wager.KindBet, 2500), Reversed: true}, 10000)
	require.Equal(t, wager.FailureReferenceAmountMismatch, s.Transaction.FailureCode())

	// The wallet's own rules come before the Reference's: a stranger's wallet.
	stranger, err := domainwallet.Rehydrate(domainwallet.State{ID: wallet, PlayerID: uuid.New(), Balance: brl(t, 10000), Version: 2, LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at})
	require.NoError(t, err)
	got, err := reversal(t, wager.KindRefund, 2500).SettleAgainst(wager.Reference{}, stranger, uuid.New(), settleAt, false)
	require.NoError(t, err)
	require.Equal(t, wager.FailurePlayerMismatch, got.Transaction.FailureCode())
}

// --- a WIN that names a Reference -----------------------------------------------------------

func TestAWinNamingTheBetOfTheSameRoundIsAcceptedWhateverItsAmount(t *testing.T) {
	bet2500 := processed(t, wager.KindBet, 2500)

	s := settleAgainst(t, winOf(t, 9000, "transaction-100"), found(bet2500), 10000)

	require.Equal(t, wager.StatusProcessed, s.Transaction.Status(), "a prize is not the stake")
	require.Equal(t, int64(19000), s.Wallet.Balance().Minor())
	require.Equal(t, domainwallet.DirectionCredit, s.Entry.Direction())
	require.Equal(t, bet2500.ID(), s.Transaction.External().ReferenceTransactionID)
}

func TestAWinIsNeverAReversal(t *testing.T) {
	// The BET was already refunded; a WIN naming it is still a prize, not a second reversal.
	refunded := wager.Reference{Found: true, Transaction: processed(t, wager.KindBet, 2500), Reversed: true}
	s := settleAgainst(t, winOf(t, 100, "transaction-100"), refunded, 10000)
	require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
}

func TestAWinNamingABetOfAnotherRoundIsAMismatch(t *testing.T) {
	p := betParams(t)
	p.RoundID = "another-round"
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k", CorrelationID: "c", At: at})
	require.NoError(t, err)
	bet, err := tx.Process(wager.Result{Balance: brl(t, 1), WalletVersion: 2}, at)
	require.NoError(t, err)
	s := settleAgainst(t, winOf(t, 100, "transaction-100"), found(bet), 10000)
	requireRejected(t, s, wager.FailureReferenceMismatch, "")
}

func TestARefundThatWouldOverflowTheBalanceIsRefused(t *testing.T) {
	s, err := reversal(t, wager.KindRefund, 2500).SettleAgainst(found(processed(t, wager.KindBet, 2500)), walletOf(t, math.MaxInt64), uuid.New(), settleAt, false)
	require.NoError(t, err)
	require.Equal(t, wager.FailureBalanceLimitExceeded, s.Transaction.FailureCode())
}

// --- waiting for a Reference that has not arrived ---------------------------------------

func settleWaiting(t *testing.T, tx wager.Transaction, ref wager.Reference) wager.Settlement {
	t.Helper()
	s, err := tx.SettleAgainst(ref, walletOf(t, 10000), uuid.New(), settleAt, true)
	require.NoError(t, err)
	return s
}

func requireWaiting(t *testing.T, s wager.Settlement, msg string) {
	t.Helper()
	require.True(t, s.Waiting(), msg)
	require.Equal(t, wager.StatusPendingReference, s.Transaction.Status(), msg)
	require.False(t, s.Entry.IsValid(), msg)
	require.Equal(t, int64(10000), s.Wallet.Balance().Minor(), msg)
	require.Equal(t, int64(2), s.Wallet.Version(), msg)
	require.Empty(t, s.Transaction.FailureCode(), msg)
}

func TestAReferenceThatIsNotThereYetIsWaitedFor(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		for _, tx := range []wager.Transaction{reversal(t, wager.KindRefund, 2500), reversal(t, wager.KindRollback, 2500), winOf(t, 100, "transaction-100")} {
			requireWaiting(t, settleWaiting(t, tx, wager.Reference{}), string(tx.Kind()))
		}
	})
	t.Run("itself still waiting", func(t *testing.T) {
		_, err := bet(t, 2500).AwaitReference(settleAt)
		require.ErrorIs(t, err, wager.ErrNoReference, "a bet names no reference, so it cannot be the one waiting")
		p := betParams(t)
		p.Kind, p.ReferenceExternalTransactionID = wager.KindWin, "x"
		w, err := wager.New(wager.NewParams{ID: uuid.New(), Request: newRequest(t, p), IdempotencyKey: "k", CorrelationID: "c", At: at})
		require.NoError(t, err)
		w, err = w.AwaitReference(settleAt)
		require.NoError(t, err)
		requireWaiting(t, settleWaiting(t, reversal(t, wager.KindRefund, 2500), found(w)), "a reference that waits for its own")
	})
}

func TestWaitingTakesNothingFromTheRulesOfTheWallet(t *testing.T) {
	stranger, err := domainwallet.Rehydrate(domainwallet.State{ID: wallet, PlayerID: uuid.New(), Balance: brl(t, 10000), Version: 2, LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at})
	require.NoError(t, err)
	s, err := reversal(t, wager.KindRefund, 2500).SettleAgainst(wager.Reference{}, stranger, uuid.New(), settleAt, true)
	require.NoError(t, err)
	require.Equal(t, wager.FailurePlayerMismatch, s.Transaction.FailureCode(), "refused at once, there is nothing to wait for")
}

func TestAReferenceThatEndedWithoutSuccessIsNotWaitedFor(t *testing.T) {
	for _, status := range []wager.Status{wager.StatusRejected, wager.StatusFailed} {
		s := settleWaiting(t, reversal(t, wager.KindRefund, 2500), found(ended(t, status)))
		requireRejected(t, s, wager.FailureReferenceNotProcessed, string(status))
	}
}

func TestAnOperationAlreadyWaitingStaysAsItIsWhileItMayWait(t *testing.T) {
	waiting := settleWaiting(t, reversal(t, wager.KindRefund, 2500), wager.Reference{}).Transaction

	again, err := waiting.SettleAgainst(wager.Reference{}, walletOf(t, 10000), uuid.New(), settleAt.Add(time.Minute), true)

	require.NoError(t, err)
	requireWaiting(t, again, "still not there")
	require.Equal(t, waiting.UpdatedAt(), again.Transaction.UpdatedAt(), "a try that changes nothing changes nothing")
}

func TestAWaitingOperationIsAppliedWhenItsReferenceArrives(t *testing.T) {
	waiting := settleWaiting(t, reversal(t, wager.KindRefund, 2500), wager.Reference{}).Transaction

	s, err := waiting.SettleAgainst(found(processed(t, wager.KindBet, 2500)), walletOf(t, 10000), uuid.New(), settleAt.Add(time.Minute), true)

	require.NoError(t, err)
	require.Equal(t, wager.StatusProcessed, s.Transaction.Status())
	require.Equal(t, int64(12500), s.Wallet.Balance().Minor())
	require.True(t, s.Entry.IsValid())
}

func TestAWaitingOperationIsRefusedWhenItMayNotWaitAnyMore(t *testing.T) {
	waiting := settleWaiting(t, reversal(t, wager.KindRefund, 2500), wager.Reference{}).Transaction
	later := settleAt.Add(time.Hour)

	never, err := waiting.SettleAgainst(wager.Reference{}, walletOf(t, 10000), uuid.New(), later, false)
	require.NoError(t, err)
	requireRejected(t, never, wager.FailureReferenceNotFound, "it never appeared")

	stuck, err := waiting.SettleAgainst(found(ended(t, wager.StatusPending)), walletOf(t, 10000), uuid.New(), later, false)
	require.NoError(t, err)
	requireRejected(t, stuck, wager.FailureReferenceNotProcessed, "it appeared and never completed")
}
