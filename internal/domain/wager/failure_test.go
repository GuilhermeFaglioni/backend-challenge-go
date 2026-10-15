package wager_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

func TestTheCatalogHasEveryDocumentedCodeWithItsClass(t *testing.T) {
	want := map[wager.FailureCode]wager.FailureClass{
		wager.FailureInsufficientFunds:         wager.ClassDefinitive,
		wager.FailureBalanceLimitExceeded:      wager.ClassDefinitive,
		wager.FailureReversalInsufficientFunds: wager.ClassDefinitive,
		wager.FailureWalletNotFound:            wager.ClassDefinitive,
		wager.FailureCurrencyMismatch:          wager.ClassCorrectable,
		wager.FailurePlayerMismatch:            wager.ClassCorrectable,
		wager.FailureReferenceNotFound:         wager.ClassDefinitive,
		wager.FailureReferenceNotProcessed:     wager.ClassDefinitive,
		wager.FailureReferenceKindInvalid:      wager.ClassCorrectable,
		wager.FailureReferenceMismatch:         wager.ClassCorrectable,
		wager.FailureReferenceAmountMismatch:   wager.ClassCorrectable,
		wager.FailureReferenceAlreadyReversed:  wager.ClassDefinitive,
		wager.FailureInternalPermanent:         wager.ClassDefinitive,
	}
	got := map[wager.FailureCode]wager.FailureClass{}
	for _, code := range wager.FailureCodes() {
		got[code] = code.Class()
	}
	require.Equal(t, want, got)
}

func TestTheStringsOfTheCodesAreStable(t *testing.T) {
	// Clients branch on these strings: they are part of the contract.
	for code, text := range map[wager.FailureCode]string{
		wager.FailureInsufficientFunds:         "INSUFFICIENT_FUNDS",
		wager.FailureBalanceLimitExceeded:      "BALANCE_LIMIT_EXCEEDED",
		wager.FailureReversalInsufficientFunds: "REVERSAL_INSUFFICIENT_FUNDS",
		wager.FailureWalletNotFound:            "WALLET_NOT_FOUND",
		wager.FailureCurrencyMismatch:          "CURRENCY_MISMATCH",
		wager.FailurePlayerMismatch:            "PLAYER_MISMATCH",
		wager.FailureReferenceNotFound:         "REFERENCE_NOT_FOUND",
		wager.FailureReferenceNotProcessed:     "REFERENCE_NOT_PROCESSED",
		wager.FailureReferenceKindInvalid:      "REFERENCE_KIND_INVALID",
		wager.FailureReferenceMismatch:         "REFERENCE_MISMATCH",
		wager.FailureReferenceAmountMismatch:   "REFERENCE_AMOUNT_MISMATCH",
		wager.FailureReferenceAlreadyReversed:  "REFERENCE_ALREADY_REVERSED",
		wager.FailureInternalPermanent:         "INTERNAL_PERMANENT_FAILURE",
	} {
		require.Equal(t, text, string(code))
	}
}

func TestOnlyTheInternalPermanentFailureIsNotARejection(t *testing.T) {
	for _, code := range wager.FailureCodes() {
		require.True(t, code.IsValid(), code)
		require.Equal(t, code != wager.FailureInternalPermanent, code.IsRejection(), code)
	}
	for _, code := range []wager.FailureCode{"", "SOMETHING_ELSE", "insufficient_funds", "INSUFFICIENT_FUNDS "} {
		require.False(t, code.IsValid(), "%q", code)
		require.False(t, code.IsRejection(), "%q", code)
		require.Equal(t, wager.FailureClass(""), code.Class(), "%q", code)
	}
}

func TestEachCodeIsListedOnce(t *testing.T) {
	seen := map[wager.FailureCode]bool{}
	for _, code := range wager.FailureCodes() {
		require.False(t, seen[code], "%s is listed twice", code)
		seen[code] = true
	}
}

// The catalog in the code and the one in ARCHITECTURE.md (section 11) are the same
// list with the same classes: a code added to one and not the other fails the build.
func TestTheCatalogMatchesTheDocumentation(t *testing.T) {
	text, err := os.ReadFile("../../../ARCHITECTURE.md")
	require.NoError(t, err)
	_, section, found := strings.Cut(string(text), "### Catálogo de `failureCode`")
	require.True(t, found, "the catalog section is missing from ARCHITECTURE.md")
	section, _, _ = strings.Cut(section, "\n\n\"Corrigível\"")

	row := regexp.MustCompile("(?m)^\\| `([A-Z_]+)` \\| (corrigível|definitivo) \\|")
	documented := map[wager.FailureCode]wager.FailureClass{}
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		class := wager.ClassDefinitive
		if m[2] == "corrigível" {
			class = wager.ClassCorrectable
		}
		documented[wager.FailureCode(m[1])] = class
	}
	require.NotEmpty(t, documented)

	inCode := map[wager.FailureCode]wager.FailureClass{}
	for _, code := range wager.FailureCodes() {
		inCode[code] = code.Class()
	}
	require.Equal(t, documented, inCode)
}
