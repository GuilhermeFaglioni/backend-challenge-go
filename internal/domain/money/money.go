package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Errors returned by this package. Callers classify them with errors.Is.
var (
	// ErrInvalidFormat: the text is not exactly an amount with two decimals.
	ErrInvalidFormat = errors.New("money: amount must be digits, a dot and exactly two decimals, such as 25.00")
	// ErrNegative: external input must not be negative.
	ErrNegative = errors.New("money: amount must not be negative")
	// ErrOverflow: the result does not fit in the representation.
	ErrOverflow = errors.New("money: amount out of range")
	// ErrUnknownCurrency: not in the supported ISO 4217 list.
	ErrUnknownCurrency = errors.New("money: unknown currency")
	// ErrCurrencyMismatch: arithmetic or comparison between different currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrUninitialized: the zero value of Money was used.
	ErrUninitialized = errors.New("money: uninitialized value")
)

// Currency is an ISO 4217 code from the closed list below. All of them have two
// decimals, which is the scale this package fixes (ADR 0003).
type Currency string

// The supported currencies. Currencies with another scale (JPY, KWD) are not
// supported.
const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

var supported = map[Currency]bool{BRL: true, USD: true, EUR: true}

// ParseCurrency validates an ISO 4217 code. Codes are upper case.
func ParseCurrency(code string) (Currency, error) {
	c := Currency(code)
	if !supported[c] {
		return "", fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return c, nil
}

func (c Currency) String() string { return string(c) }

// Money is an immutable amount in minor units (cents) with its currency.
//
// The range is the whole of int64, about 92 quadrillion units. Values are
// comparable with ==. The zero value is invalid and rejected by every operation;
// build values with Parse, FromMinor or Zero.
type Money struct {
	minor    int64
	currency Currency
}

// Zero returns zero in the given currency.
func Zero(c Currency) (Money, error) { return FromMinor(0, c) }

// FromMinor builds a value from minor units. Unlike Parse it accepts negative
// amounts: they appear in internal calculations such as differences, though
// never in a wallet balance.
func FromMinor(minor int64, c Currency) (Money, error) {
	if !supported[c] {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, string(c))
	}
	return Money{minor: minor, currency: c}, nil
}

// Parse reads an amount received from outside the system. The text must match
// exactly `^(0|[1-9][0-9]*)\.[0-9]{2}$`: nothing is normalized, rounded or
// guessed, so each amount has one textual form, which the idempotency hash
// relies on. Negative amounts are ErrNegative.
func Parse(amount string, c Currency) (Money, error) {
	if !supported[c] {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, string(c))
	}

	text, negative := amount, false
	if len(text) > 0 && text[0] == '-' {
		text, negative = text[1:], true
	}
	whole, fraction, ok := splitAmount(text)
	if !ok {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidFormat, amount)
	}
	if negative {
		return Money{}, fmt.Errorf("%w: %q", ErrNegative, amount)
	}

	// Accumulate with an overflow check at every step; there is no float.
	var value int64
	for i := 0; i < len(whole); i++ {
		digit := int64(whole[i] - '0')
		if value > (math.MaxInt64-digit)/10 {
			return Money{}, fmt.Errorf("%w: %q", ErrOverflow, amount)
		}
		value = value*10 + digit
	}
	cents := int64(fraction[0]-'0')*10 + int64(fraction[1]-'0')
	if value > (math.MaxInt64-cents)/100 {
		return Money{}, fmt.Errorf("%w: %q", ErrOverflow, amount)
	}
	return Money{minor: value*100 + cents, currency: c}, nil
}

// splitAmount checks the shape digits "." two digits, without a leading zero
// unless the integer part is exactly "0". Only ASCII digits are digits.
func splitAmount(s string) (whole, fraction string, ok bool) {
	dot := -1
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '.':
			if dot != -1 {
				return "", "", false
			}
			dot = i
		case c < '0' || c > '9':
			return "", "", false
		}
	}
	if dot < 1 || len(s)-dot-1 != 2 {
		return "", "", false
	}
	whole, fraction = s[:dot], s[dot+1:]
	if len(whole) > 1 && whole[0] == '0' {
		return "", "", false
	}
	return whole, fraction, true
}

// IsValid reports whether the value was built by this package, that is, it is
// not the zero value.
func (m Money) IsValid() bool { return m.currency != "" }

// Minor returns the amount in minor units.
func (m Money) Minor() int64 { return m.minor }

// Currency returns the currency.
func (m Money) Currency() Currency { return m.currency }

func (m Money) IsZero() bool     { return m.IsValid() && m.minor == 0 }
func (m Money) IsNegative() bool { return m.minor < 0 }
func (m Money) IsPositive() bool { return m.minor > 0 }

// String returns the amount with two decimals, such as "25.00" or "-0.05",
// without the currency. For a value Parse accepts, it returns the input.
func (m Money) String() string {
	if !m.IsValid() {
		return "<invalid money>"
	}
	var magnitude uint64
	if m.minor < 0 {
		// -(minor+1) is never negative here, so the conversion is exact, and the
		// +1 afterwards is what makes math.MinInt64 representable.
		magnitude = uint64(-(m.minor + 1)) + 1 //nolint:gosec // see above
	} else {
		magnitude = uint64(m.minor)
	}
	text := strconv.FormatUint(magnitude/100, 10) + "." + string(rune('0'+magnitude%100/10)) + string(rune('0'+magnitude%10))
	if m.minor < 0 {
		return "-" + text
	}
	return text
}

// both checks that two values are usable together.
func (m Money) both(other Money) error {
	if !m.IsValid() || !other.IsValid() {
		return ErrUninitialized
	}
	if m.currency != other.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return nil
}

// Add returns m + other.
func (m Money) Add(other Money) (Money, error) {
	if err := m.both(other); err != nil {
		return Money{}, err
	}
	if (other.minor > 0 && m.minor > math.MaxInt64-other.minor) ||
		(other.minor < 0 && m.minor < math.MinInt64-other.minor) {
		return Money{}, ErrOverflow
	}
	return Money{minor: m.minor + other.minor, currency: m.currency}, nil
}

// Sub returns m - other. The result may be negative.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.both(other); err != nil {
		return Money{}, err
	}
	if (other.minor > 0 && m.minor < math.MinInt64+other.minor) ||
		(other.minor < 0 && m.minor > math.MaxInt64+other.minor) {
		return Money{}, ErrOverflow
	}
	return Money{minor: m.minor - other.minor, currency: m.currency}, nil
}

// Neg returns -m. The most negative amount has no positive counterpart.
func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, ErrUninitialized
	}
	if m.minor == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Compare returns -1, 0 or 1 as m is less than, equal to or greater than other.
func (m Money) Compare(other Money) (int, error) {
	if err := m.both(other); err != nil {
		return 0, err
	}
	switch {
	case m.minor < other.minor:
		return -1, nil
	case m.minor > other.minor:
		return 1, nil
	}
	return 0, nil
}

// wire is the external representation: {"amount":"25.00","currency":"BRL"}.
// The amount is a string so no JSON number, and no float, is ever involved.
type wire struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON encodes as {"amount":"25.00","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, ErrUninitialized
	}
	return json.Marshal(wire{Amount: m.String(), Currency: string(m.currency)})
}

// UnmarshalJSON decodes external input with the strictness of Parse: the amount
// must be a string in the exact format and non-negative, the currency must be
// supported, both fields are required, unknown fields are refused and nothing may
// follow the object. Every failure is classifiable with errors.Is, and none echoes
// the decoder's internal types. On error the receiver is left as the zero value,
// never half filled.
//
// The writing side is deliberately not the mirror image: MarshalJSON also writes
// negative amounts (a reconciliation difference can be negative), which this method
// then refuses, as it does for any negative input.
func (m *Money) UnmarshalJSON(data []byte) error {
	*m = Money{}

	var in struct {
		Amount   *string `json:"amount"`
		Currency *string `json:"currency"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return fmt.Errorf("%w: expected an object with the string fields amount and currency", ErrInvalidFormat)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the object", ErrInvalidFormat)
	}
	if in.Amount == nil || in.Currency == nil {
		return fmt.Errorf("%w: both amount and currency are required", ErrInvalidFormat)
	}
	currency, err := ParseCurrency(*in.Currency)
	if err != nil {
		return err
	}
	parsed, err := Parse(*in.Amount, currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
