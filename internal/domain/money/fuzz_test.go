package money_test

import (
	"testing"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

// FuzzParse checks the property that makes idempotency hashing safe: there is a
// single textual form for each amount, so anything Parse accepts prints back as
// exactly the same text. The seeds run as ordinary tests on every build.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"", "0.00", "25.00", "0.01", "007.00", "25", "25.0", "-5.00", "1e2", "NaN",
		"92233720368547758.07", "92233720368547758.08", "１.00", "1.0\x00", " 1.00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		m, err := money.Parse(input, money.BRL)
		if err != nil {
			if m.IsValid() {
				t.Fatalf("Parse(%q) failed but returned a usable value %v", input, m)
			}
			return
		}
		if got := m.String(); got != input {
			t.Fatalf("Parse(%q) accepted an amount that prints as %q", input, got)
		}
		if m.IsNegative() {
			t.Fatalf("Parse(%q) returned a negative amount from external input", input)
		}
	})
}
