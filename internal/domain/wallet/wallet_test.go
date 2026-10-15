package wallet_test

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// opened returns a wallet opened with the given balance (zero opens it empty).
func opened(t *testing.T, minor int64) wallet.Wallet {
	t.Helper()
	if minor == 0 {
		w, err := wallet.Open(uuid.New(), uuid.New(), money.BRL, at)
		require.NoError(t, err)
		return w
	}
	w, _, err := wallet.OpenWithBalance(uuid.New(), uuid.New(), brl(t, minor), uuid.New(), at)
	require.NoError(t, err)
	return w
}

func movement(t *testing.T, minor int64) wallet.Movement {
	t.Helper()
	return wallet.Movement{EntryID: uuid.New(), TransactionID: uuid.New(), Amount: brl(t, minor), At: at.Add(time.Minute)}
}

// --- creation ----------------------------------------------------------------------

func TestAWalletOpensEmptyAtVersionOne(t *testing.T) {
	id, player := uuid.New(), uuid.New()
	w, err := wallet.Open(id, player, money.USD, at)
	require.NoError(t, err)

	require.True(t, w.IsValid())
	require.Equal(t, id, w.ID())
	require.Equal(t, player, w.PlayerID())
	require.Equal(t, money.USD, w.Currency())
	require.Equal(t, int64(0), w.Balance().Minor())
	require.True(t, w.Balance().IsZero())
	require.Equal(t, int64(1), w.Version())
	require.Equal(t, at, w.CreatedAt())
	require.Equal(t, at, w.UpdatedAt())
	_, has := w.LastTransactionID()
	require.False(t, has, "an empty wallet has had no movement")
}

func TestAWalletOpensFundedByItsOpeningTransaction(t *testing.T) {
	id, player, opening := uuid.New(), uuid.New(), uuid.New()
	w, entry, err := wallet.OpenWithBalance(id, player, brl(t, 5000), opening, at)
	require.NoError(t, err)

	require.Equal(t, int64(5000), w.Balance().Minor())
	require.Equal(t, int64(1), w.Version(), "opening is not a movement on top of an existing wallet: the version is still 1")
	last, has := w.LastTransactionID()
	require.True(t, has)
	require.Equal(t, opening, last)

	require.True(t, entry.IsValid())
	require.Equal(t, id, entry.WalletID())
	require.Equal(t, opening, entry.TransactionID())
	require.Equal(t, wallet.DirectionCredit, entry.Direction())
	require.Equal(t, int64(5000), entry.Amount().Minor())
	require.Equal(t, int64(0), entry.BalanceBefore().Minor())
	require.Equal(t, int64(5000), entry.BalanceAfter().Minor())
	require.Equal(t, at, entry.CreatedAt())
}

func TestOpeningRefusesBadInput(t *testing.T) {
	id, player := uuid.New(), uuid.New()

	t.Run("empty wallet", func(t *testing.T) {
		_, err := wallet.Open(uuid.Nil, player, money.BRL, at)
		require.ErrorIs(t, err, wallet.ErrInvalidID)
		_, err = wallet.Open(id, uuid.Nil, money.BRL, at)
		require.ErrorIs(t, err, wallet.ErrInvalidID)
		_, err = wallet.Open(id, player, "XXX", at)
		require.ErrorIs(t, err, money.ErrUnknownCurrency)
		_, err = wallet.Open(id, player, money.BRL, time.Time{})
		require.ErrorIs(t, err, wallet.ErrInvalidTime)
	})
	t.Run("funded wallet", func(t *testing.T) {
		opening := uuid.New()
		cases := map[string]func() (wallet.Wallet, wallet.LedgerEntry, error){
			"nil wallet id": func() (wallet.Wallet, wallet.LedgerEntry, error) {
				return wallet.OpenWithBalance(uuid.Nil, player, brl(t, 100), opening, at)
			},
			"nil player": func() (wallet.Wallet, wallet.LedgerEntry, error) {
				return wallet.OpenWithBalance(id, uuid.Nil, brl(t, 100), opening, at)
			},
			"nil opening transaction": func() (wallet.Wallet, wallet.LedgerEntry, error) {
				return wallet.OpenWithBalance(id, player, brl(t, 100), uuid.Nil, at)
			},
			"zero time": func() (wallet.Wallet, wallet.LedgerEntry, error) {
				return wallet.OpenWithBalance(id, player, brl(t, 100), opening, time.Time{})
			},
		}
		for name, open := range cases {
			w, e, err := open()
			require.Error(t, err, name)
			require.False(t, w.IsValid(), name)
			require.False(t, e.IsValid(), name)
		}
	})
	t.Run("funding must be a positive amount", func(t *testing.T) {
		for name, amount := range map[string]money.Money{
			"zero": brl(t, 0), "negative": brl(t, -100), "uninitialized": {},
		} {
			_, _, err := wallet.OpenWithBalance(id, player, amount, uuid.New(), at)
			require.ErrorIs(t, err, wallet.ErrInvalidAmount, name)
		}
	})
}

// --- rehydration -------------------------------------------------------------------

func validState(t *testing.T) wallet.State {
	t.Helper()
	return wallet.State{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, 4200), Version: 7,
		LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at.Add(time.Hour),
	}
}

func TestRehydratingRestoresTheStateAsIs(t *testing.T) {
	s := validState(t)
	w, err := wallet.Rehydrate(s)
	require.NoError(t, err)

	require.Equal(t, s.ID, w.ID())
	require.Equal(t, s.PlayerID, w.PlayerID())
	require.Equal(t, s.Balance, w.Balance())
	require.Equal(t, int64(7), w.Version(), "rehydration applies no movement: the version is not touched")
	last, has := w.LastTransactionID()
	require.True(t, has)
	require.Equal(t, s.LastTransactionID, last)
	require.Equal(t, s.CreatedAt, w.CreatedAt())
	require.Equal(t, s.UpdatedAt, w.UpdatedAt())
}

func TestRehydratingAnEmptyWalletWithoutAnyMovement(t *testing.T) {
	s := validState(t)
	s.Balance, s.Version, s.LastTransactionID, s.UpdatedAt = brl(t, 0), 1, uuid.Nil, s.CreatedAt
	w, err := wallet.Rehydrate(s)
	require.NoError(t, err)
	_, has := w.LastTransactionID()
	require.False(t, has)
}

func TestRehydratingRefusesWhatTheDatabaseCouldNeverHold(t *testing.T) {
	cases := map[string]struct {
		mutate func(*wallet.State)
		want   error
	}{
		"nil id":                {func(s *wallet.State) { s.ID = uuid.Nil }, wallet.ErrInvalidID},
		"nil player":            {func(s *wallet.State) { s.PlayerID = uuid.Nil }, wallet.ErrInvalidID},
		"negative balance":      {func(s *wallet.State) { s.Balance = mustBRL(-1) }, wallet.ErrInvalidBalance},
		"uninitialized balance": {func(s *wallet.State) { s.Balance = money.Money{} }, wallet.ErrInvalidBalance},
		"version zero":          {func(s *wallet.State) { s.Version = 0 }, wallet.ErrInvalidVersion},
		"negative version":      {func(s *wallet.State) { s.Version = -3 }, wallet.ErrInvalidVersion},
		"moved but no last transaction": {func(s *wallet.State) { s.LastTransactionID = uuid.Nil },
			wallet.ErrInvalidVersion},
		"zero created":     {func(s *wallet.State) { s.CreatedAt = time.Time{} }, wallet.ErrInvalidTime},
		"zero updated":     {func(s *wallet.State) { s.UpdatedAt = time.Time{} }, wallet.ErrInvalidTime},
		"updated < create": {func(s *wallet.State) { s.UpdatedAt = s.CreatedAt.Add(-time.Second) }, wallet.ErrInvalidTime},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := validState(t)
			c.mutate(&s)
			w, err := wallet.Rehydrate(s)
			require.ErrorIs(t, err, c.want)
			require.False(t, w.IsValid())
		})
	}
}

// --- movements ---------------------------------------------------------------------

func TestACreditRaisesTheBalanceAndTheVersionAndWritesAnEntry(t *testing.T) {
	w := opened(t, 10000)
	m := movement(t, 2500)

	next, entry, err := w.Credit(m)
	require.NoError(t, err)

	require.Equal(t, int64(12500), next.Balance().Minor())
	require.Equal(t, int64(2), next.Version())
	last, _ := next.LastTransactionID()
	require.Equal(t, m.TransactionID, last)
	require.Equal(t, m.At, next.UpdatedAt())
	require.Equal(t, w.CreatedAt(), next.CreatedAt())

	require.Equal(t, m.EntryID, entry.ID())
	require.Equal(t, w.ID(), entry.WalletID())
	require.Equal(t, m.TransactionID, entry.TransactionID())
	require.Equal(t, wallet.DirectionCredit, entry.Direction())
	require.Equal(t, int64(2500), entry.Amount().Minor())
	require.Equal(t, int64(10000), entry.BalanceBefore().Minor())
	require.Equal(t, int64(12500), entry.BalanceAfter().Minor())
	require.Equal(t, m.At, entry.CreatedAt())
}

func TestADebitLowersTheBalanceAndRaisesTheVersionAndWritesAnEntry(t *testing.T) {
	w := opened(t, 10000)
	m := movement(t, 2500)

	next, entry, err := w.Debit(m)
	require.NoError(t, err)

	require.Equal(t, int64(7500), next.Balance().Minor())
	require.Equal(t, int64(2), next.Version())
	last, _ := next.LastTransactionID()
	require.Equal(t, m.TransactionID, last)
	require.Equal(t, wallet.DirectionDebit, entry.Direction())
	require.Equal(t, int64(10000), entry.BalanceBefore().Minor())
	require.Equal(t, int64(7500), entry.BalanceAfter().Minor())
}

func TestAMovementNeverChangesTheWalletItWasMadeOn(t *testing.T) {
	w := opened(t, 10000)
	_, _, err := w.Debit(movement(t, 1))
	require.NoError(t, err)
	_, _, err = w.Credit(movement(t, 1))
	require.NoError(t, err)

	require.Equal(t, int64(10000), w.Balance().Minor())
	require.Equal(t, int64(1), w.Version())
}

func TestADebitMayTakeTheBalanceExactlyToZero(t *testing.T) {
	next, entry, err := opened(t, 10000).Debit(movement(t, 10000))
	require.NoError(t, err)
	require.True(t, next.Balance().IsZero())
	require.Equal(t, int64(0), entry.BalanceAfter().Minor())
}

func TestADebitBeyondTheBalanceIsRefusedAndChangesNothing(t *testing.T) {
	w := opened(t, 10000)
	for _, over := range []int64{10001, 10000 + 1, math.MaxInt64} {
		next, entry, err := w.Debit(movement(t, over))
		require.ErrorIs(t, err, wallet.ErrInsufficientFunds, "amount %d", over)
		require.False(t, next.IsValid(), "a refused movement must not hand back a usable wallet")
		require.False(t, entry.IsValid())
	}
	require.Equal(t, int64(10000), w.Balance().Minor())
	require.Equal(t, int64(1), w.Version())
}

func TestAnEmptyWalletCannotBeDebited(t *testing.T) {
	_, _, err := opened(t, 0).Debit(movement(t, 1))
	require.ErrorIs(t, err, wallet.ErrInsufficientFunds)
}

func TestAMovementNeedsAPositiveAmount(t *testing.T) {
	w := opened(t, 10000)
	for name, amount := range map[string]money.Money{
		"zero": brl(t, 0), "negative": brl(t, -500), "uninitialized": {},
	} {
		m := movement(t, 1)
		m.Amount = amount
		_, _, err := w.Debit(m)
		require.ErrorIs(t, err, wallet.ErrInvalidAmount, "debit %s", name)
		_, _, err = w.Credit(m)
		require.ErrorIs(t, err, wallet.ErrInvalidAmount, "credit %s", name)
	}
}

func TestAMovementMustBeInTheWalletsCurrency(t *testing.T) {
	w := opened(t, 10000)
	usd, err := money.FromMinor(100, money.USD)
	require.NoError(t, err)
	m := movement(t, 1)
	m.Amount = usd

	_, _, err = w.Debit(m)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
	_, _, err = w.Credit(m)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
}

func TestACreditThatWouldOverflowIsRefusedAndChangesNothing(t *testing.T) {
	w, err := wallet.Rehydrate(wallet.State{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, math.MaxInt64-10), Version: 2,
		LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at,
	})
	require.NoError(t, err)

	_, _, err = w.Credit(movement(t, 11))
	require.ErrorIs(t, err, money.ErrOverflow)
	next, _, err := w.Credit(movement(t, 10))
	require.NoError(t, err, "right up to the limit is still fine")
	require.Equal(t, int64(math.MaxInt64), next.Balance().Minor())
}

func TestAVersionThatCannotGrowIsRefused(t *testing.T) {
	w, err := wallet.Rehydrate(wallet.State{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, 100), Version: math.MaxInt64,
		LastTransactionID: uuid.New(), CreatedAt: at, UpdatedAt: at,
	})
	require.NoError(t, err)
	_, _, err = w.Credit(movement(t, 1))
	require.ErrorIs(t, err, wallet.ErrInvalidVersion)
}

func TestAMovementNeedsItsIdentifiersAndATime(t *testing.T) {
	w := opened(t, 10000)
	cases := map[string]func(*wallet.Movement){
		"nil entry id":       func(m *wallet.Movement) { m.EntryID = uuid.Nil },
		"nil transaction id": func(m *wallet.Movement) { m.TransactionID = uuid.Nil },
		"zero time":          func(m *wallet.Movement) { m.At = time.Time{} },
	}
	wants := map[string]error{"nil entry id": wallet.ErrInvalidID, "nil transaction id": wallet.ErrInvalidID, "zero time": wallet.ErrInvalidTime}
	for name, mutate := range cases {
		m := movement(t, 100)
		mutate(&m)
		_, _, err := w.Debit(m)
		require.ErrorIs(t, err, wants[name], "debit %s", name)
		_, _, err = w.Credit(m)
		require.ErrorIs(t, err, wants[name], "credit %s", name)
	}
}

func TestTheZeroWalletRefusesEverything(t *testing.T) {
	var w wallet.Wallet
	require.False(t, w.IsValid())
	_, _, err := w.Debit(movement(t, 100))
	require.ErrorIs(t, err, wallet.ErrUninitialized)
	_, _, err = w.Credit(movement(t, 100))
	require.ErrorIs(t, err, wallet.ErrUninitialized)
}

// --- the invariants over many movements --------------------------------------------

// Whatever the sequence of debits and credits, the balance never goes negative, the
// version rises by exactly one per accepted movement and never otherwise, and the
// entries chain: each one starts where the previous one ended.
func TestInvariantsHoldOverRandomSequencesOfMovements(t *testing.T) {
	for seed := uint64(1); seed <= 50; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7919)) //nolint:gosec // a seeded generator keeps the sequences reproducible; it is not security-related
		w := opened(t, int64(rng.IntN(100_000)))
		expected := w.Balance().Minor()
		accepted := int64(0)
		var previousAfter *int64

		for i := 0; i < 200; i++ {
			amount := int64(rng.IntN(60_000) + 1)
			m := movement(t, amount)
			var (
				next  wallet.Wallet
				entry wallet.LedgerEntry
				err   error
			)
			credit := rng.IntN(2) == 0
			if credit {
				next, entry, err = w.Credit(m)
			} else {
				next, entry, err = w.Debit(m)
			}

			switch {
			case err == nil:
				accepted++
				if credit {
					expected += amount
				} else {
					expected -= amount
				}
				require.Equal(t, entry.BalanceBefore().Minor(), w.Balance().Minor(), "seed %d step %d", seed, i)
				if previousAfter != nil {
					require.Equal(t, *previousAfter, entry.BalanceBefore().Minor(), "the entries must chain")
				}
				after := entry.BalanceAfter().Minor()
				previousAfter = &after
				w = next
			case !credit:
				require.ErrorIs(t, err, wallet.ErrInsufficientFunds)
				require.Greater(t, amount, w.Balance().Minor())
			default:
				require.NoError(t, err, "a credit this small cannot fail")
			}

			require.GreaterOrEqual(t, w.Balance().Minor(), int64(0), "seed %d step %d", seed, i)
			require.Equal(t, expected, w.Balance().Minor())
			require.Equal(t, 1+accepted, w.Version())
		}
	}
}
