package wager_test

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

var (
	player = uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7")
	wallet = uuid.MustParse("3fa85f64-5717-4562-b3fc-2c963f66afa6")
)

func betParams(t *testing.T) wager.RequestParams {
	t.Helper()
	return wager.RequestParams{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123",
		PlayerID: player, WalletID: wallet, RoundID: "round-987", GameID: "fortune-chimp",
		Kind: wager.KindBet, Amount: brl(t, 2500),
	}
}

func refundParams(t *testing.T) wager.RequestParams {
	t.Helper()
	p := betParams(t)
	p.Kind, p.ReferenceExternalTransactionID = wager.KindRefund, "transaction-100"
	return p
}

func newRequest(t *testing.T, p wager.RequestParams) wager.Request {
	t.Helper()
	r, err := wager.NewRequest(p)
	require.NoError(t, err)
	return r
}

func TestARequestKeepsWhatItWasBuiltWith(t *testing.T) {
	p := refundParams(t)
	r := newRequest(t, p)
	require.True(t, r.IsValid())
	require.Equal(t, p.ProviderID, r.ProviderID())
	require.Equal(t, p.ExternalTransactionID, r.ExternalTransactionID())
	require.Equal(t, p.PlayerID, r.PlayerID())
	require.Equal(t, p.WalletID, r.WalletID())
	require.Equal(t, p.RoundID, r.RoundID())
	require.Equal(t, p.GameID, r.GameID())
	require.Equal(t, p.Kind, r.Kind())
	require.Equal(t, p.Amount, r.Amount())
	require.Equal(t, p.ReferenceExternalTransactionID, r.ReferenceExternalTransactionID())
	require.False(t, wager.Request{}.IsValid())
}

// --- the canonical form and the hash, pinned --------------------------------------

// These constants were computed with a different tool (Python's json with sorted keys
// and no spaces, and hashlib), not with this code. If one changes, every stored
// idempotency hash is invalidated: that must never happen by accident.
const (
	betCanonical = `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"},` +
		`"playerId":"7c9e6679-7425-40de-944b-e07fc1f90ae7","providerId":"provider-a","roundId":"round-987","walletId":"3fa85f64-5717-4562-b3fc-2c963f66afa6"}`
	betHash = "44f2060bdb1d8000f4364130a1bc2b3e20b92062e53e801d13ad247b7b4a2a1d"

	refundCanonical = `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"REFUND","money":{"amount":"25.00","currency":"BRL"},` +
		`"playerId":"7c9e6679-7425-40de-944b-e07fc1f90ae7","providerId":"provider-a","referenceExternalTransactionId":"transaction-100",` +
		`"roundId":"round-987","walletId":"3fa85f64-5717-4562-b3fc-2c963f66afa6"}`
	refundHash = "29073bbbf213d123a1c8c913e09a6dc7d30f79ff1537a7d614b491ac3577dce4"

	unicodeCanonical = `{"externalTransactionId":"transaction-123","gameId":"fortuna-macaco-ção <&>","kind":"BET","money":{"amount":"25.00","currency":"BRL"},` +
		`"playerId":"7c9e6679-7425-40de-944b-e07fc1f90ae7","providerId":"provider-a","roundId":"round-987","walletId":"3fa85f64-5717-4562-b3fc-2c963f66afa6"}`
	unicodeHash = "8c7569cd463b50972ce10dd7e4534f12bde1c241d73238bdee66ea7ad9030bd0"
)

func TestTheCanonicalJSONAndTheHashAreFixed(t *testing.T) {
	unicode := betParams(t)
	unicode.GameID = "fortuna-macaco-ção <&>"
	for name, c := range map[string]struct {
		params    wager.RequestParams
		canonical string
		hash      string
	}{
		"a bet":                        {betParams(t), betCanonical, betHash},
		"a refund, with its reference": {refundParams(t), refundCanonical, refundHash},
		"UTF-8 and HTML characters are written as they are": {unicode, unicodeCanonical, unicodeHash},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRequest(t, c.params)
			require.Equal(t, c.canonical, string(r.CanonicalJSON()), "keys sorted, no spaces, no absent fields")
			require.Equal(t, wager.PayloadHash(c.hash), r.Hash())

			sum := sha256.Sum256([]byte(c.canonical))
			require.Equal(t, hex.EncodeToString(sum[:]), string(r.Hash()), "the hash is the SHA-256 of exactly that JSON")
		})
	}
}

func TestTheHashIsLowerCaseHexOfSixtyFourCharacters(t *testing.T) {
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), string(newRequest(t, betParams(t)).Hash()))
}

// Any change of a business field must change the hash, or the conflict detection
// would take a different request for a repetition.
func TestEveryBusinessFieldChangesTheHash(t *testing.T) {
	base := refundParams(t)
	baseHash := newRequest(t, base).Hash()
	eur, err := money.FromMinor(2500, money.EUR)
	require.NoError(t, err)

	mutations := map[string]func(*wager.RequestParams){
		"provider":    func(p *wager.RequestParams) { p.ProviderID = "provider-b" },
		"external id": func(p *wager.RequestParams) { p.ExternalTransactionID = "transaction-124" },
		"player":      func(p *wager.RequestParams) { p.PlayerID = uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae8") },
		"wallet":      func(p *wager.RequestParams) { p.WalletID = uuid.MustParse("3fa85f64-5717-4562-b3fc-2c963f66afa7") },
		"round":       func(p *wager.RequestParams) { p.RoundID = "round-988" },
		"game":        func(p *wager.RequestParams) { p.GameID = "fortune-tiger" },
		"kind":        func(p *wager.RequestParams) { p.Kind = wager.KindRollback },
		"amount":      func(p *wager.RequestParams) { p.Amount = brl(t, 2501) },
		"currency":    func(p *wager.RequestParams) { p.Amount = eur },
		"reference":   func(p *wager.RequestParams) { p.ReferenceExternalTransactionID = "transaction-101" },
	}
	seen := map[wager.PayloadHash]string{baseHash: "the base request"}
	for name, mutate := range mutations {
		p := base
		mutate(&p)
		h := newRequest(t, p).Hash()
		require.NotEqual(t, baseHash, h, "changing the %s did not change the hash", name)
		require.NotContains(t, seen, h, "changing the %s collides with %s", name, seen[h])
		seen[h] = name
	}
}

func TestAnAbsentReferenceIsNotTheSameAsAnEmptyOne(t *testing.T) {
	// A WIN may or may not reference a bet; the two requests are different requests.
	without := betParams(t)
	without.Kind = wager.KindWin
	with := without
	with.ReferenceExternalTransactionID = "transaction-100"
	require.NotEqual(t, newRequest(t, without).Hash(), newRequest(t, with).Hash())
	require.NotContains(t, string(newRequest(t, without).CanonicalJSON()), "referenceExternalTransactionId", "absent fields are left out, not written empty")
}

func TestTheHashIsTheSameEveryTimeAndOnEveryGoroutine(t *testing.T) {
	r := newRequest(t, refundParams(t))
	var wg sync.WaitGroup
	hashes := make(chan wager.PayloadHash, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hashes <- newRequest(t, refundParams(t)).Hash()
		}()
	}
	wg.Wait()
	close(hashes)
	for h := range hashes {
		require.Equal(t, r.Hash(), h)
	}
}

// --- value rules per kind ---------------------------------------------------------

func TestTheZeroPolicyByKind(t *testing.T) {
	zero := brl(t, 0)
	one := brl(t, 1)
	for _, kind := range []wager.Kind{wager.KindBet, wager.KindWin, wager.KindRefund, wager.KindRollback} {
		p := betParams(t)
		p.Kind = kind
		if kind == wager.KindRefund || kind == wager.KindRollback {
			p.ReferenceExternalTransactionID = "transaction-100"
		}
		p.Amount = zero
		_, err := wager.NewRequest(p)
		require.ErrorIs(t, err, wager.ErrInvalidAmount, "%s needs a positive amount", kind)
		p.Amount = mustBRL(-100)
		_, err = wager.NewRequest(p)
		require.ErrorIs(t, err, wager.ErrInvalidAmount, "%s cannot be negative", kind)
		p.Amount = money.Money{}
		_, err = wager.NewRequest(p)
		require.ErrorIs(t, err, wager.ErrInvalidAmount, "%s needs an amount", kind)
		p.Amount = one
		_, err = wager.NewRequest(p)
		require.NoError(t, err, "%s accepts 0.01", kind)
	}

	loss := betParams(t)
	loss.Kind = wager.KindLoss
	loss.Amount = zero
	_, err := wager.NewRequest(loss)
	require.NoError(t, err, "a LOSS is exactly 0.00")
	for _, amount := range []money.Money{one, mustBRL(-1), {}} {
		loss.Amount = amount
		_, err := wager.NewRequest(loss)
		require.ErrorIs(t, err, wager.ErrInvalidAmount, "a LOSS moves nothing: %v", amount)
	}
}

func TestAnExternalRequestCanNeverBeAnOpening(t *testing.T) {
	p := betParams(t)
	p.Kind = wager.KindOpening
	_, err := wager.NewRequest(p)
	require.ErrorIs(t, err, wager.ErrInvalidKind, "OPENING is reserved to the internal opening of a wallet")
}

func TestUnknownKindsAreRefused(t *testing.T) {
	for _, kind := range []wager.Kind{"", "bet", "TRANSFER", "BET "} {
		p := betParams(t)
		p.Kind = kind
		_, err := wager.NewRequest(p)
		require.ErrorIs(t, err, wager.ErrInvalidKind, "%q", kind)
	}
}

func TestReferencesByKind(t *testing.T) {
	cases := map[string]struct {
		kind      wager.Kind
		reference string
		want      error
	}{
		"refund needs one":          {wager.KindRefund, "", wager.ErrReferenceRequired},
		"rollback needs one":        {wager.KindRollback, "", wager.ErrReferenceRequired},
		"refund with one":           {wager.KindRefund, "transaction-100", nil},
		"rollback with one":         {wager.KindRollback, "transaction-100", nil},
		"a win may reference a bet": {wager.KindWin, "transaction-100", nil},
		"a win may stand alone":     {wager.KindWin, "", nil},
		"a bet references nothing":  {wager.KindBet, "transaction-100", wager.ErrReferenceNotAllowed},
		"a bet without one":         {wager.KindBet, "", nil},
		"a loss references nothing": {wager.KindLoss, "transaction-100", wager.ErrReferenceNotAllowed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := betParams(t)
			p.Kind, p.ReferenceExternalTransactionID = c.kind, c.reference
			if c.kind == wager.KindLoss {
				p.Amount = brl(t, 0)
			}
			_, err := wager.NewRequest(p)
			if c.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, c.want)
		})
	}
}

func TestARequestCannotReferenceItself(t *testing.T) {
	p := refundParams(t)
	p.ReferenceExternalTransactionID = p.ExternalTransactionID
	_, err := wager.NewRequest(p)
	require.ErrorIs(t, err, wager.ErrReferenceNotAllowed)
}

// --- field rules ------------------------------------------------------------------

func TestIdentifiersAreValidated(t *testing.T) {
	long := strings.Repeat("a", 129)
	bad := map[string]func(*wager.RequestParams){
		"empty provider":                func(p *wager.RequestParams) { p.ProviderID = "" },
		"provider upper case":           func(p *wager.RequestParams) { p.ProviderID = "Provider-A" },
		"provider with a space":         func(p *wager.RequestParams) { p.ProviderID = "provider a" },
		"provider starting with a dash": func(p *wager.RequestParams) { p.ProviderID = "-provider" },
		"provider too long":             func(p *wager.RequestParams) { p.ProviderID = strings.Repeat("a", 64) },
		"empty external id":             func(p *wager.RequestParams) { p.ExternalTransactionID = "" },
		"external id too long":          func(p *wager.RequestParams) { p.ExternalTransactionID = long },
		"external id with a newline":    func(p *wager.RequestParams) { p.ExternalTransactionID = "a\nb" },
		"external id with padding":      func(p *wager.RequestParams) { p.ExternalTransactionID = " transaction-1" },
		"external id not UTF-8":         func(p *wager.RequestParams) { p.ExternalTransactionID = "bad\xffid" },
		"external id with NUL":          func(p *wager.RequestParams) { p.ExternalTransactionID = "a\x00b" },
		"empty round":                   func(p *wager.RequestParams) { p.RoundID = "" },
		"round too long":                func(p *wager.RequestParams) { p.RoundID = long },
		"empty game":                    func(p *wager.RequestParams) { p.GameID = "" },
		"game with a tab":               func(p *wager.RequestParams) { p.GameID = "fortune\tchimp" },
		"reference too long":            func(p *wager.RequestParams) { p.Kind, p.ReferenceExternalTransactionID = wager.KindWin, long },
		"reference with padding":        func(p *wager.RequestParams) { p.Kind, p.ReferenceExternalTransactionID = wager.KindWin, "ref " },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			p := betParams(t)
			mutate(&p)
			r, err := wager.NewRequest(p)
			require.Error(t, err)
			require.False(t, r.IsValid())
		})
	}

	t.Run("the limits themselves are fine", func(t *testing.T) {
		p := betParams(t)
		p.ProviderID = "a" + strings.Repeat("b", 62)
		p.ExternalTransactionID = strings.Repeat("é", 128) // 128 characters, 256 bytes
		p.RoundID = "r"
		_, err := wager.NewRequest(p)
		require.NoError(t, err)
	})
}

func TestIdentifiersOfDifferentKindsReportDifferentErrors(t *testing.T) {
	p := betParams(t)
	p.ProviderID = "Bad Provider"
	_, err := wager.NewRequest(p)
	require.ErrorIs(t, err, wager.ErrInvalidProvider)

	p = betParams(t)
	p.RoundID = ""
	_, err = wager.NewRequest(p)
	require.ErrorIs(t, err, wager.ErrInvalidIdentifier)
}

func TestTheUUIDsCannotBeNil(t *testing.T) {
	p := betParams(t)
	p.PlayerID = uuid.Nil
	_, err := wager.NewRequest(p)
	require.ErrorIs(t, err, wager.ErrInvalidID)
	p = betParams(t)
	p.WalletID = uuid.Nil
	_, err = wager.NewRequest(p)
	require.ErrorIs(t, err, wager.ErrInvalidID)
}
