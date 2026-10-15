package money_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

func mustParse(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	require.NoError(t, err)
	return m
}

func mustMinor(t *testing.T, minor int64, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, currency)
	require.NoError(t, err)
	return m
}

func TestParseAcceptsOnlyTheExactFormat(t *testing.T) {
	tests := []struct {
		input     string
		wantMinor int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.99", 99},
		{"1.00", 100},
		{"25.00", 2500},
		{"1000.50", 100050},
		{"92233720368547758.07", math.MaxInt64}, // the largest representable amount
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			m, err := money.Parse(tt.input, money.BRL)
			require.NoError(t, err)
			require.Equal(t, tt.wantMinor, m.Minor())
			require.Equal(t, money.BRL, m.Currency())
			require.Equal(t, tt.input, m.String(), "the only valid text is the canonical one")
		})
	}
}

func TestParseRejectsEverythingElse(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{"empty", "", money.ErrInvalidFormat},
		{"NaN", "NaN", money.ErrInvalidFormat},
		{"Infinity", "Infinity", money.ErrInvalidFormat},
		{"negative infinity", "-Infinity", money.ErrInvalidFormat},
		{"scientific", "1e2", money.ErrInvalidFormat},
		{"scientific with fraction", "1.00e2", money.ErrInvalidFormat},
		{"no fraction", "25", money.ErrInvalidFormat},
		{"one decimal", "25.0", money.ErrInvalidFormat},
		{"three decimals", "25.000", money.ErrInvalidFormat},
		{"excess scale never rounds", "0.005", money.ErrInvalidFormat},
		{"no integer part", ".50", money.ErrInvalidFormat},
		{"no fraction digits", "25.", money.ErrInvalidFormat},
		{"leading zero", "007.00", money.ErrInvalidFormat},
		{"plus sign", "+5.00", money.ErrInvalidFormat},
		{"leading space", " 25.00", money.ErrInvalidFormat},
		{"trailing space", "25.00 ", money.ErrInvalidFormat},
		{"trailing newline", "25.00\n", money.ErrInvalidFormat},
		{"thousands separator", "1,000.00", money.ErrInvalidFormat},
		{"comma as decimal separator", "25,00", money.ErrInvalidFormat},
		{"letters", "abc", money.ErrInvalidFormat},
		{"two dots", "1.0.00", money.ErrInvalidFormat},
		{"non-ASCII digits", "２５.00", money.ErrInvalidFormat},
		{"hex", "0x10.00", money.ErrInvalidFormat},
		{"just a sign", "-", money.ErrInvalidFormat},
		{"negative", "-5.00", money.ErrNegative},
		{"negative zero", "-0.00", money.ErrNegative},
		{"overflow by one minor unit", "92233720368547758.08", money.ErrOverflow},
		{"overflow in the integer part", "92233720368547759.00", money.ErrOverflow},
		{"far beyond the range", "99999999999999999999999999.99", money.ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.Parse(tt.input, money.BRL)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestParseRejectsAnUnknownOrUninitializedCurrency(t *testing.T) {
	_, err := money.Parse("1.00", money.Currency("XXX"))
	require.ErrorIs(t, err, money.ErrUnknownCurrency)

	_, err = money.Parse("1.00", money.Currency(""))
	require.ErrorIs(t, err, money.ErrUnknownCurrency)

	_, err = money.Parse("1.00", money.Currency("brl"))
	require.ErrorIs(t, err, money.ErrUnknownCurrency, "ISO 4217 codes are upper case")
}

func TestParseCurrency(t *testing.T) {
	for _, code := range []string{"BRL", "USD", "EUR"} {
		c, err := money.ParseCurrency(code)
		require.NoError(t, err)
		require.Equal(t, code, c.String())
	}
	for _, code := range []string{"", "brl", "BR", "BRLL", "JPY", "XXX", " BRL"} {
		_, err := money.ParseCurrency(code)
		require.ErrorIs(t, err, money.ErrUnknownCurrency, "code %q", code)
	}
}

func TestTheZeroValueIsInvalid(t *testing.T) {
	var zero money.Money
	require.False(t, zero.IsValid())

	_, err := zero.Add(mustMinor(t, 1, money.BRL))
	require.ErrorIs(t, err, money.ErrUninitialized)
	_, err = mustMinor(t, 1, money.BRL).Add(zero)
	require.ErrorIs(t, err, money.ErrUninitialized)
	_, err = zero.Sub(zero)
	require.ErrorIs(t, err, money.ErrUninitialized)
	_, err = zero.Neg()
	require.ErrorIs(t, err, money.ErrUninitialized)
	_, err = zero.Compare(zero)
	require.ErrorIs(t, err, money.ErrUninitialized)
	_, err = json.Marshal(zero)
	require.ErrorIs(t, err, money.ErrUninitialized)

	require.True(t, mustMinor(t, 0, money.BRL).IsValid())
}

func TestZeroByCurrency(t *testing.T) {
	z, err := money.Zero(money.USD)
	require.NoError(t, err)
	require.True(t, z.IsZero())
	require.True(t, z.IsValid())
	require.Equal(t, money.USD, z.Currency())
	require.Equal(t, "0.00", z.String())

	_, err = money.Zero(money.Currency("JPY"))
	require.ErrorIs(t, err, money.ErrUnknownCurrency)
}

func TestFromMinorAllowsNegativeAmountsForInternalCalculations(t *testing.T) {
	m := mustMinor(t, -1234, money.BRL)
	require.Equal(t, int64(-1234), m.Minor())
	require.True(t, m.IsNegative())
	require.Equal(t, "-12.34", m.String())

	_, err := money.FromMinor(1, money.Currency("XXX"))
	require.ErrorIs(t, err, money.ErrUnknownCurrency)
}

func TestFormattingIsExactAtTheLimits(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{math.MaxInt64, "92233720368547758.07"},
		{math.MinInt64, "-92233720368547758.08"},
		{-1, "-0.01"},
		{-100, "-1.00"},
		{5, "0.05"},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, mustMinor(t, tt.minor, money.BRL).String())
	}
}

func TestArithmetic(t *testing.T) {
	a := mustParse(t, "10.50", money.BRL)
	b := mustParse(t, "0.75", money.BRL)

	sum, err := a.Add(b)
	require.NoError(t, err)
	require.Equal(t, "11.25", sum.String())

	diff, err := a.Sub(b)
	require.NoError(t, err)
	require.Equal(t, "9.75", diff.String())

	// A difference may be negative; the wallet, not Money, forbids a negative balance.
	neg, err := b.Sub(a)
	require.NoError(t, err)
	require.Equal(t, "-9.75", neg.String())
	require.True(t, neg.IsNegative())

	negated, err := a.Neg()
	require.NoError(t, err)
	require.Equal(t, "-10.50", negated.String())
	back, err := negated.Neg()
	require.NoError(t, err)
	require.Equal(t, a, back)
}

func TestArithmeticDetectsOverflow(t *testing.T) {
	max := mustMinor(t, math.MaxInt64, money.BRL)
	min := mustMinor(t, math.MinInt64, money.BRL)
	one := mustMinor(t, 1, money.BRL)
	minusOne := mustMinor(t, -1, money.BRL)

	_, err := max.Add(one)
	require.ErrorIs(t, err, money.ErrOverflow)
	_, err = min.Add(minusOne)
	require.ErrorIs(t, err, money.ErrOverflow)
	_, err = min.Sub(one)
	require.ErrorIs(t, err, money.ErrOverflow)
	_, err = max.Sub(minusOne)
	require.ErrorIs(t, err, money.ErrOverflow)
	_, err = min.Neg()
	require.ErrorIs(t, err, money.ErrOverflow, "-MinInt64 does not fit")

	// The edges themselves are fine.
	got, err := max.Add(minusOne)
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64-1), got.Minor())
	got, err = min.Add(one)
	require.NoError(t, err)
	require.Equal(t, int64(math.MinInt64+1), got.Minor())
	got, err = max.Neg()
	require.NoError(t, err)
	require.Equal(t, int64(-math.MaxInt64), got.Minor())
	got, err = max.Sub(max)
	require.NoError(t, err)
	require.True(t, got.IsZero())
}

func TestArithmeticAndComparisonRequireTheSameCurrency(t *testing.T) {
	brl := mustParse(t, "1.00", money.BRL)
	usd := mustParse(t, "1.00", money.USD)

	_, err := brl.Add(usd)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
	_, err = brl.Sub(usd)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
	_, err = brl.Compare(usd)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
}

func TestCompare(t *testing.T) {
	small := mustParse(t, "1.00", money.BRL)
	big := mustParse(t, "2.00", money.BRL)

	for _, tt := range []struct {
		a, b money.Money
		want int
	}{{small, big, -1}, {big, small, 1}, {small, small, 0}} {
		got, err := tt.a.Compare(tt.b)
		require.NoError(t, err)
		require.Equal(t, tt.want, got)
	}

	// Equal values are comparable with ==, so they can be map keys or compared in tests.
	require.Equal(t, mustParse(t, "1.00", money.BRL), small)
	require.True(t, small == mustMinor(t, 100, money.BRL))
	require.False(t, small == mustMinor(t, 100, money.USD))

	// Negative numbers order correctly too.
	got, err := mustMinor(t, -5, money.BRL).Compare(mustMinor(t, 3, money.BRL))
	require.NoError(t, err)
	require.Equal(t, -1, got)
}

func TestJSONRoundTripIsExact(t *testing.T) {
	m := mustParse(t, "25.00", money.BRL)

	data, err := json.Marshal(m)
	require.NoError(t, err)
	require.JSONEq(t, `{"amount":"25.00","currency":"BRL"}`, string(data))
	require.Equal(t, `{"amount":"25.00","currency":"BRL"}`, string(data), "field order is fixed")

	var back money.Money
	require.NoError(t, json.Unmarshal(data, &back))
	require.Equal(t, m, back)

	// Inside a larger document, as the API and the events use it.
	type payload struct {
		Money money.Money `json:"money"`
	}
	data, err = json.Marshal(payload{Money: m})
	require.NoError(t, err)
	require.JSONEq(t, `{"money":{"amount":"25.00","currency":"BRL"}}`, string(data))
	var p payload
	require.NoError(t, json.Unmarshal(data, &p))
	require.Equal(t, m, p.Money)
}

func TestJSONRejectsAnythingThatIsNotAnExactAmount(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"amount as a JSON number", `{"amount":25.00,"currency":"BRL"}`},
		{"amount as an integer number", `{"amount":25,"currency":"BRL"}`},
		{"missing amount", `{"currency":"BRL"}`},
		{"missing currency", `{"amount":"25.00"}`},
		{"empty object", `{}`},
		{"null", `null`},
		{"a string instead of an object", `"25.00"`},
		{"wrong scale", `{"amount":"25.0","currency":"BRL"}`},
		{"negative", `{"amount":"-5.00","currency":"BRL"}`},
		{"unknown currency", `{"amount":"5.00","currency":"XXX"}`},
		{"lower-case currency", `{"amount":"5.00","currency":"brl"}`},
		{"unknown field", `{"amount":"5.00","currency":"BRL","extra":true}`},
		{"scientific notation", `{"amount":"1e2","currency":"BRL"}`},
		{"overflow", `{"amount":"92233720368547758.08","currency":"BRL"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m money.Money
			require.Error(t, json.Unmarshal([]byte(tt.json), &m))
			require.False(t, m.IsValid(), "a failed decode must not leave a usable value behind")
		})
	}
}

// A failed decode must not leave an old, valid amount behind: a decoder reused
// for a second document would otherwise carry the first one's value into a
// request that was rejected.
func TestAFailedDecodeResetsAnAlreadyValidValue(t *testing.T) {
	m := mustParse(t, "5.00", money.BRL)
	require.Error(t, json.Unmarshal([]byte(`{"amount":"oops","currency":"BRL"}`), &m))
	require.False(t, m.IsValid())
	require.Equal(t, money.Money{}, m)
}

// Money can leave the system negative (the reconciliation reports the stored
// balance minus the rebuilt one, which can be below zero), but it can never enter
// the system negative. The JSON round trip is exact for every value that is valid
// as external input; a negative value is written and then refused when read back.
func TestNegativeAmountsAreWrittenButNeverAccepted(t *testing.T) {
	for _, minor := range []int64{-5, -100, math.MinInt64} {
		m := mustMinor(t, minor, money.BRL)
		data, err := json.Marshal(m)
		require.NoError(t, err)
		require.Contains(t, string(data), `"amount":"-`)

		var back money.Money
		require.ErrorIs(t, json.Unmarshal(data, &back), money.ErrNegative)
		require.False(t, back.IsValid())
	}
}

// Whatever is wrong with the shape of the document, the error is classifiable
// and does not leak the decoder's internal types.
func TestDecodeErrorsAreClassifiableAndHideInternals(t *testing.T) {
	for name, input := range map[string]string{
		"amount as a number":   `{"amount":25,"currency":"BRL"}`,
		"currency as a number": `{"amount":"25.00","currency":1}`,
		"an array":             `[]`,
		"a bare string":        `"25.00"`,
		"a number":             `25`,
		"unknown field":        `{"amount":"5.00","currency":"BRL","extra":true}`,
		"null":                 `null`,
		"null amount":          `{"amount":null,"currency":"BRL"}`,
		"missing both":         `{}`,
		"missing currency":     `{"amount":"5.00"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var m money.Money
			err := json.Unmarshal([]byte(input), &m)
			require.ErrorIs(t, err, money.ErrInvalidFormat)
			require.NotContains(t, err.Error(), "struct", "the message must not expose the decoder's types")
			require.NotContains(t, err.Error(), "json:")
		})
	}
}

// json.Unmarshal rejects trailing data on its own, but UnmarshalJSON is public
// and can be called directly.
func TestUnmarshalJSONRejectsTrailingData(t *testing.T) {
	var m money.Money
	require.ErrorIs(t, m.UnmarshalJSON([]byte(`{"amount":"1.00","currency":"BRL"} junk`)), money.ErrInvalidFormat)
	require.False(t, m.IsValid())
	require.ErrorIs(t, m.UnmarshalJSON([]byte(`{"amount":"1.00","currency":"BRL"}{"amount":"2.00","currency":"BRL"}`)), money.ErrInvalidFormat)
	require.False(t, m.IsValid())
}
