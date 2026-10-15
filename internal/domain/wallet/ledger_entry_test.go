package wallet_test

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

func brl(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	return m
}

// mustBRL is brl for table fixtures built before a *testing.T exists.
func mustBRL(minor int64) money.Money {
	m, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		panic(err)
	}
	return m
}

var at = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// entryParams returns the parameters of a valid credit of 25.00 on top of 100.00.
func entryParams(t *testing.T) wallet.LedgerEntryParams {
	t.Helper()
	return wallet.LedgerEntryParams{
		ID:            uuid.New(),
		WalletID:      uuid.New(),
		TransactionID: uuid.New(),
		Direction:     wallet.DirectionCredit,
		Amount:        brl(t, 2500),
		BalanceBefore: brl(t, 10000),
		BalanceAfter:  brl(t, 12500),
		CreatedAt:     at,
	}
}

func TestALedgerEntryKeepsWhatItWasBuiltWith(t *testing.T) {
	p := entryParams(t)
	e, err := wallet.NewLedgerEntry(p)
	require.NoError(t, err)

	require.True(t, e.IsValid())
	require.Equal(t, p.ID, e.ID())
	require.Equal(t, p.WalletID, e.WalletID())
	require.Equal(t, p.TransactionID, e.TransactionID())
	require.Equal(t, wallet.DirectionCredit, e.Direction())
	require.Equal(t, p.Amount, e.Amount())
	require.Equal(t, p.BalanceBefore, e.BalanceBefore())
	require.Equal(t, p.BalanceAfter, e.BalanceAfter())
	require.Equal(t, p.CreatedAt, e.CreatedAt())
}

func TestTheZeroLedgerEntryIsInvalid(t *testing.T) {
	var e wallet.LedgerEntry
	require.False(t, e.IsValid())
}

func TestTheArithmeticOfAnEntryFollowsItsDirection(t *testing.T) {
	t.Run("credit adds", func(t *testing.T) {
		p := entryParams(t)
		_, err := wallet.NewLedgerEntry(p)
		require.NoError(t, err)

		p.BalanceAfter = brl(t, 7500) // what a debit would have produced
		_, err = wallet.NewLedgerEntry(p)
		require.ErrorIs(t, err, wallet.ErrInconsistentEntry)
	})
	t.Run("debit subtracts", func(t *testing.T) {
		p := entryParams(t)
		p.Direction = wallet.DirectionDebit
		p.BalanceAfter = brl(t, 7500)
		_, err := wallet.NewLedgerEntry(p)
		require.NoError(t, err)

		p.BalanceAfter = brl(t, 12500) // what a credit would have produced
		_, err = wallet.NewLedgerEntry(p)
		require.ErrorIs(t, err, wallet.ErrInconsistentEntry)
	})
	t.Run("an off-by-one cent is refused", func(t *testing.T) {
		for _, after := range []int64{12499, 12501} {
			p := entryParams(t)
			p.BalanceAfter = brl(t, after)
			_, err := wallet.NewLedgerEntry(p)
			require.ErrorIs(t, err, wallet.ErrInconsistentEntry, "after=%d", after)
		}
	})
}

func TestAnEntryThatOverflowsIsRefused(t *testing.T) {
	p := entryParams(t)
	p.BalanceBefore = brl(t, math.MaxInt64-100)
	p.Amount = brl(t, 200)
	p.BalanceAfter = brl(t, math.MaxInt64)
	_, err := wallet.NewLedgerEntry(p)
	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestADebitCannotTakeTheBalanceBelowZero(t *testing.T) {
	p := entryParams(t)
	p.Direction = wallet.DirectionDebit
	p.Amount = brl(t, 10001)
	p.BalanceAfter = brl(t, -1)
	_, err := wallet.NewLedgerEntry(p)
	require.ErrorIs(t, err, wallet.ErrInvalidBalance)
}

func TestAnEntryRefusesWhatIsNotAProperEntry(t *testing.T) {
	cases := map[string]struct {
		mutate func(*wallet.LedgerEntryParams)
		want   error
	}{
		"nil id":              {func(p *wallet.LedgerEntryParams) { p.ID = uuid.Nil }, wallet.ErrInvalidID},
		"nil wallet":          {func(p *wallet.LedgerEntryParams) { p.WalletID = uuid.Nil }, wallet.ErrInvalidID},
		"nil transaction":     {func(p *wallet.LedgerEntryParams) { p.TransactionID = uuid.Nil }, wallet.ErrInvalidID},
		"unknown direction":   {func(p *wallet.LedgerEntryParams) { p.Direction = "SIDEWAYS" }, wallet.ErrInvalidDirection},
		"empty direction":     {func(p *wallet.LedgerEntryParams) { p.Direction = "" }, wallet.ErrInvalidDirection},
		"zero amount":         {func(p *wallet.LedgerEntryParams) { p.Amount = mustBRL(0) }, wallet.ErrInvalidAmount},
		"negative amount":     {func(p *wallet.LedgerEntryParams) { p.Amount = mustBRL(-2500) }, wallet.ErrInvalidAmount},
		"uninitialized money": {func(p *wallet.LedgerEntryParams) { p.Amount = money.Money{} }, wallet.ErrInvalidAmount},
		"uninitialized before": {func(p *wallet.LedgerEntryParams) { p.BalanceBefore = money.Money{} },
			wallet.ErrInvalidBalance},
		"uninitialized after": {func(p *wallet.LedgerEntryParams) { p.BalanceAfter = money.Money{} },
			wallet.ErrInvalidBalance},
		"negative before": {func(p *wallet.LedgerEntryParams) { p.BalanceBefore = mustBRL(-1) }, wallet.ErrInvalidBalance},
		"zero time":       {func(p *wallet.LedgerEntryParams) { p.CreatedAt = time.Time{} }, wallet.ErrInvalidTime},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := entryParams(t)
			c.mutate(&p)
			e, err := wallet.NewLedgerEntry(p)
			require.ErrorIs(t, err, c.want)
			require.False(t, e.IsValid(), "a refused entry must not be usable")
		})
	}
}

func TestAnEntryMixingCurrenciesIsRefused(t *testing.T) {
	usd, err := money.FromMinor(2500, money.USD)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*wallet.LedgerEntryParams){
		"amount": func(p *wallet.LedgerEntryParams) { p.Amount = usd },
		"before": func(p *wallet.LedgerEntryParams) { p.BalanceBefore = usd },
		"after":  func(p *wallet.LedgerEntryParams) { p.BalanceAfter = usd },
	} {
		t.Run(name, func(t *testing.T) {
			p := entryParams(t)
			mutate(&p)
			_, err := wallet.NewLedgerEntry(p)
			require.ErrorIs(t, err, money.ErrCurrencyMismatch)
		})
	}
}

func TestAnEntryIsStoredInUTC(t *testing.T) {
	p := entryParams(t)
	p.CreatedAt = time.Date(2026, 10, 14, 9, 0, 0, 0, time.FixedZone("BRT", -3*60*60))
	e, err := wallet.NewLedgerEntry(p)
	require.NoError(t, err)
	require.Equal(t, time.UTC, e.CreatedAt().Location())
	require.True(t, p.CreatedAt.Equal(e.CreatedAt()), "the instant must not change")
}
