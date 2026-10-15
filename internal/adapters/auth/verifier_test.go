package auth_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
)

type rig struct {
	idp   *fakeIDP
	clock *clock
	v     *auth.Verifier
	key   keyPair
}

func newRig(t *testing.T) *rig {
	t.Helper()
	idp := newFakeIDP(t)
	r := &rig{idp: idp, clock: newClock(), key: idp.newKey("kid-1", true)}
	r.v = r.start(t, auth.Options{})
	return r
}

// start creates a verifier against the fake provider and starts it.
func (r *rig) start(t *testing.T, o auth.Options) *auth.Verifier {
	t.Helper()
	o.Issuer, o.DiscoveryURL, o.Audience = r.idp.IssuerURL(), r.idp.IssuerURL(), testAudience
	o.Now = r.clock.Now
	v, err := auth.New(o)
	require.NoError(t, err)
	t.Cleanup(v.Close)
	require.NoError(t, v.Start(t.Context()))
	return v
}

func reasonOf(t *testing.T, err error) auth.Reason {
	t.Helper()
	var authErr *auth.Error
	require.ErrorAs(t, err, &authErr, "want an *auth.Error, got %v", err)
	return authErr.Reason
}

func (r *rig) verify(token string) (auth.Principal, error) {
	return r.v.Verify(context.Background(), token)
}

func TestAValidProviderTokenYieldsItsPrincipal(t *testing.T) {
	r := newRig(t)
	p, err := r.verify(sign(t, r.key, claimsFor(r.idp, r.clock)))
	require.NoError(t, err)

	require.Equal(t, "provider-a", p.ProviderID())
	require.Equal(t, "provider-a", p.ClientID())
	require.Equal(t, "service-account-provider-a", p.Subject())
	require.True(t, p.HasRole(auth.RoleProvider))
	require.False(t, p.HasRole(auth.RoleInternal))
}

func TestAnInternalTokenHasNoProviderID(t *testing.T) {
	r := newRig(t)
	claims := with(with(claimsFor(r.idp, r.clock), "provider_id", nil), "azp", "internal-service")
	claims["resource_access"] = map[string]any{testAudience: map[string]any{"roles": []string{"internal"}}}

	p, err := r.verify(sign(t, r.key, claims))
	require.NoError(t, err)
	require.True(t, p.HasRole(auth.RoleInternal))
	require.Empty(t, p.ProviderID())
}

func TestTheAudienceMayBeAListAndUnknownRolesAreIgnored(t *testing.T) {
	r := newRig(t)
	claims := with(claimsFor(r.idp, r.clock), "aud", []string{"account", testAudience})
	claims["resource_access"] = map[string]any{
		testAudience: map[string]any{"roles": []string{"provider", "superuser", "provider"}},
		"account":    map[string]any{"roles": []string{"internal"}}, // another client's roles must not count
	}
	p, err := r.verify(sign(t, r.key, claims))
	require.NoError(t, err)
	require.Equal(t, []auth.Role{auth.RoleProvider}, p.Roles())
}

func TestATokenWithNoRolesIsAuthenticatedButHasNoPermission(t *testing.T) {
	r := newRig(t)
	claims := with(with(claimsFor(r.idp, r.clock), "provider_id", nil), "resource_access", nil)
	p, err := r.verify(sign(t, r.key, claims))
	require.NoError(t, err)
	require.Empty(t, p.Roles())
	require.False(t, p.HasAnyRole(auth.RoleProvider, auth.RoleInternal))
}

func TestEveryKindOfBadTokenIsRejectedWithItsReason(t *testing.T) {
	r := newRig(t)
	good := claimsFor(r.idp, r.clock)
	otherKey := r.idp.newKey("kid-1", false) // same key id, different key material
	strangerKey := r.idp.newKey("kid-stranger", false)

	tests := []struct {
		name  string
		token string
		want  auth.Reason
	}{
		{"empty", "", auth.ReasonMalformed},
		{"not a JWT", "abc", auth.ReasonMalformed},
		{"three garbage parts", "a.b.c", auth.ReasonMalformed},
		{"expired beyond the leeway", sign(t, r.key, with(good, "exp", r.clock.Now().Add(-time.Minute).Unix())), auth.ReasonExpired},
		{"not yet valid", sign(t, r.key, with(good, "nbf", r.clock.Now().Add(5*time.Minute).Unix())), auth.ReasonNotYetValid},
		{"no expiry at all", sign(t, r.key, with(good, "exp", nil)), auth.ReasonInvalidClaims},
		{"wrong audience", sign(t, r.key, with(good, "aud", "someone-else")), auth.ReasonWrongAudience},
		{"no audience", sign(t, r.key, with(good, "aud", nil)), auth.ReasonWrongAudience},
		{"wrong issuer", sign(t, r.key, with(good, "iss", "http://evil.example/realms/test")), auth.ReasonWrongIssuer},
		{"no issuer", sign(t, r.key, with(good, "iss", nil)), auth.ReasonWrongIssuer},
		{"signed by a key the provider does not publish", sign(t, strangerKey, good), auth.ReasonUnknownKey},
		{"right key id, wrong key", sign(t, otherKey, good), auth.ReasonBadSignature},
		{"provider role without provider id", sign(t, r.key, with(good, "provider_id", nil)), auth.ReasonInvalidClaims},
		{"provider id with a bad shape", sign(t, r.key, with(good, "provider_id", "Provider A")), auth.ReasonInvalidClaims},
		{"provider id too long", sign(t, r.key, with(good, "provider_id", strings.Repeat("a", 80))), auth.ReasonInvalidClaims},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.verify(tt.token)
			require.Equal(t, tt.want, reasonOf(t, err))
		})
	}
}

func TestATamperedPayloadFailsTheSignature(t *testing.T) {
	r := newRig(t)
	token := sign(t, r.key, claimsFor(r.idp, r.clock))
	parts := strings.Split(token, ".")

	forged := with(claimsFor(r.idp, r.clock), "provider_id", "provider-b")
	payload, err := json.Marshal(forged)
	require.NoError(t, err)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]

	_, err = r.verify(tampered)
	require.Equal(t, auth.ReasonBadSignature, reasonOf(t, err))
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestUnsignedAndDowngradedAlgorithmsAreRefused(t *testing.T) {
	r := newRig(t)
	payload, err := json.Marshal(claimsFor(r.idp, r.clock))
	require.NoError(t, err)
	body := base64.RawURLEncoding.EncodeToString(payload)

	// "alg": "none": a token anyone can write.
	for _, header := range []string{`{"alg":"none","typ":"JWT","kid":"kid-1"}`, `{"alg":"NONE","typ":"JWT","kid":"kid-1"}`} {
		_, err := r.verify(b64(header) + "." + body + ".")
		require.Equal(t, auth.ReasonUnsupportedAlgorithm, reasonOf(t, err), header)
	}

	// The classic algorithm-confusion attack: an HMAC token whose secret is the
	// provider's *public* key, which anyone can download.
	der, err := x509.MarshalPKIXPublicKey(&r.key.key.PublicKey)
	require.NoError(t, err)
	signingInput := b64(`{"alg":"HS256","typ":"JWT","kid":"kid-1"}`) + "." + body
	mac := hmac.New(sha256.New, der)
	mac.Write([]byte(signingInput))
	_, err = r.verify(signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	require.Equal(t, auth.ReasonUnsupportedAlgorithm, reasonOf(t, err))
}

func TestATokenWithoutAKeyIDIsMalformed(t *testing.T) {
	r := newRig(t)
	payload, err := json.Marshal(claimsFor(r.idp, r.clock))
	require.NoError(t, err)
	_, err = r.verify(b64(`{"alg":"RS256","typ":"JWT"}`) + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + b64("sig"))
	require.Equal(t, auth.ReasonMalformed, reasonOf(t, err))
}

func TestTheClockLeewayAbsorbsSmallSkew(t *testing.T) {
	r := newRig(t)
	good := claimsFor(r.idp, r.clock)
	_, err := r.verify(sign(t, r.key, with(good, "exp", r.clock.Now().Add(-10*time.Second).Unix())))
	require.NoError(t, err, "10 seconds past expiry is within the 30 second leeway")
	_, err = r.verify(sign(t, r.key, with(good, "nbf", r.clock.Now().Add(10*time.Second).Unix())))
	require.NoError(t, err)
}

func TestAKeyAddedByRotationIsPickedUpOnce(t *testing.T) {
	r := newRig(t)
	rotated := r.idp.newKey("kid-2", false)
	token := sign(t, rotated, claimsFor(r.idp, r.clock))

	before := r.idp.jwksFetches.Load()
	r.idp.publish(rotated)
	_, err := r.verify(token)
	require.NoError(t, err, "an unknown key id makes the verifier look at the provider's keys again")
	require.Equal(t, before+1, r.idp.jwksFetches.Load())

	_, err = r.verify(token)
	require.NoError(t, err)
	require.Equal(t, before+1, r.idp.jwksFetches.Load(), "now it is cached")
}

// Anyone can send tokens with made-up key ids. Each must not cost the identity
// provider a request.
func TestUnknownKeyIDsCannotMakeTheServiceHammerTheProvider(t *testing.T) {
	r := newRig(t)
	stranger := r.idp.newKey("placeholder", false)
	before := r.idp.jwksFetches.Load()

	for i := 0; i < 50; i++ {
		stranger.kid = "made-up-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + time.Duration(i).String()
		_, err := r.verify(sign(t, stranger, claimsFor(r.idp, r.clock)))
		require.Equal(t, auth.ReasonUnknownKey, reasonOf(t, err))
	}
	require.Equal(t, int64(1), r.idp.jwksFetches.Load()-before, "50 unknown key ids must cost exactly one look at the provider's keys")

	// After the interval, one more look is allowed, and only one.
	r.clock.Advance(31 * time.Second)
	for i := 0; i < 20; i++ {
		stranger.kid = "later-" + time.Duration(i).String()
		_, err := r.verify(sign(t, stranger, claimsFor(r.idp, r.clock)))
		require.Equal(t, auth.ReasonUnknownKey, reasonOf(t, err))
	}
	require.Equal(t, int64(2), r.idp.jwksFetches.Load()-before, "after the interval, one more look, and only one")
}

func TestKnownKeysKeepWorkingWhileTheProviderIsDown(t *testing.T) {
	r := newRig(t)
	r.idp.jwksDown.Store(true)

	// Even after the cache is stale, the last known keys are better than refusing everyone.
	r.clock.Advance(2 * time.Hour)
	fresh := sign(t, r.key, claimsFor(r.idp, r.clock))
	_, err := r.verify(fresh)
	require.NoError(t, err)

	// A key id nobody has seen, with the provider down, is not the caller's fault.
	// (The attempt above already used this interval's refresh, so wait for the next.)
	r.clock.Advance(31 * time.Second)
	stranger := r.idp.newKey("kid-new", false)
	_, err = r.verify(sign(t, stranger, claimsFor(r.idp, r.clock)))
	require.Equal(t, auth.ReasonKeysUnavailable, reasonOf(t, err))
}

func TestAKeyRemovedByTheProviderStopsWorkingAfterTheCacheExpires(t *testing.T) {
	r := newRig(t)
	other := r.idp.newKey("kid-2", true)
	_, err := r.verify(sign(t, other, claimsFor(r.idp, r.clock)))
	require.NoError(t, err)

	r.idp.unpublish("kid-2")
	_, err = r.verify(sign(t, other, claimsFor(r.idp, r.clock)))
	require.NoError(t, err, "still cached")

	r.clock.Advance(61 * time.Minute)
	_, err = r.verify(sign(t, other, claimsFor(r.idp, r.clock)))
	require.Equal(t, auth.ReasonUnknownKey, reasonOf(t, err), "a revoked key must not be honoured forever")
}

func TestStartRefusesAMisconfiguredProvider(t *testing.T) {
	r := newRig(t)

	// The discovery document must announce the issuer the service was told to expect.
	r.idp.issuerOverride = "http://somewhere-else.example/realms/test"
	v, err := auth.New(auth.Options{Issuer: r.idp.IssuerURL(), DiscoveryURL: r.idp.IssuerURL(), Audience: testAudience, Now: r.clock.Now})
	require.NoError(t, err)
	defer v.Close()
	require.ErrorContains(t, v.Start(t.Context()), "issuer")

	// An unreachable discovery endpoint fails too.
	v2, err := auth.New(auth.Options{Issuer: "http://127.0.0.1:1/realms/x", DiscoveryURL: "http://127.0.0.1:1/realms/x", Audience: testAudience})
	require.NoError(t, err)
	defer v2.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.Error(t, v2.Start(ctx))
}

func TestNewValidatesItsOptions(t *testing.T) {
	for name, o := range map[string]auth.Options{
		"no issuer":    {DiscoveryURL: "http://x", Audience: "a"},
		"no discovery": {Issuer: "http://x", Audience: "a"},
		"no audience":  {Issuer: "http://x", DiscoveryURL: "http://x"},
		"relative url": {Issuer: "/realms/x", DiscoveryURL: "http://x", Audience: "a"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := auth.New(o)
			require.Error(t, err)
		})
	}
}

func TestVerifyBeforeStartFailsInsteadOfPanicking(t *testing.T) {
	idp := newFakeIDP(t)
	v, err := auth.New(auth.Options{Issuer: idp.IssuerURL(), DiscoveryURL: idp.IssuerURL(), Audience: testAudience})
	require.NoError(t, err)
	defer v.Close()
	_, err = v.Verify(t.Context(), "a.b.c")
	require.Error(t, err)
}

func TestConcurrentVerificationIsSafe(t *testing.T) {
	r := newRig(t)
	rotated := r.idp.newKey("kid-2", true)
	tokens := []string{
		sign(t, r.key, claimsFor(r.idp, r.clock)),
		sign(t, rotated, claimsFor(r.idp, r.clock)),
		sign(t, r.idp.newKey("kid-x", false), claimsFor(r.idp, r.clock)),
	}
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, token := range tokens {
				_, _ = r.verify(token)
			}
		}()
	}
	wg.Wait()
}

func TestErrorsNeverCarryTokenContent(t *testing.T) {
	r := newRig(t)
	marker := "SECRET-MARKER-" + strings.Repeat("z", 12)
	token := sign(t, r.key, with(claimsFor(r.idp, r.clock), "note", marker))
	tampered := token[:len(token)-4] + "AAAA"

	for _, candidate := range []string{tampered, marker, sign(t, r.key, with(with(claimsFor(r.idp, r.clock), "note", marker), "aud", "x"))} {
		_, err := r.verify(candidate)
		require.Error(t, err)
		var authErr *auth.Error
		require.True(t, errors.As(err, &authErr))
		require.NotContains(t, err.Error(), marker)
		require.NotContains(t, err.Error(), tampered[len(tampered)-30:])
	}
}

// --- the key cache under attack and under failure ---------------------------------

// waitFor polls a condition without fixed sleeps in the test body.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	require.Eventually(t, condition, 5*time.Second, 10*time.Millisecond, what)
}

// The refresh must not depend on the request that happened to trigger it: an
// attacker (or a client with a short timeout) that gives up mid-refresh would
// otherwise use up the interval's one refresh, and a real key rotation would
// never be picked up.
func TestACancelledRequestDoesNotStopTheKeysFromRefreshing(t *testing.T) {
	r := newRig(t)
	rotated := r.idp.newKey("kid-2", true)
	token := sign(t, rotated, claimsFor(r.idp, r.clock))
	r.idp.jwksDelay.Store(int64(300 * time.Millisecond))
	served := r.idp.jwksServed.Load()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := r.v.Verify(ctx, token)
	require.Equal(t, auth.ReasonKeysUnavailable, reasonOf(t, err), "the caller that gave up is told so")

	// The refresh still completes, and the next caller gets the rotated key
	// without another look at the provider (the interval's refresh was not wasted).
	waitFor(t, "the refresh to complete", func() bool { return r.idp.jwksServed.Load() > served })
	r.idp.jwksDelay.Store(0)
	fetches := r.idp.jwksFetches.Load()
	_, err = r.verify(token)
	require.NoError(t, err)
	require.Equal(t, fetches, r.idp.jwksFetches.Load(), "served from the refreshed cache")
}

// While the provider is slow to answer a refresh, tokens signed by keys the cache
// already has must keep being verified at full speed.
func TestKnownKeysAreNotBlockedBySlowRefresh(t *testing.T) {
	r := newRig(t)
	r.idp.jwksDelay.Store(int64(2 * time.Second))
	known := sign(t, r.key, claimsFor(r.idp, r.clock))
	stranger := sign(t, r.idp.newKey("kid-new", false), claimsFor(r.idp, r.clock))

	go func() { _, _ = r.v.Verify(context.Background(), stranger) }() // starts the slow refresh
	waitFor(t, "the refresh to start", func() bool { return r.idp.jwksFetches.Load() > 1 })

	start := time.Now()
	_, err := r.verify(known)
	require.NoError(t, err)
	require.Less(t, time.Since(start), 500*time.Millisecond, "a known key waited for the refresh")

	r.idp.jwksDelay.Store(0)
}

// Callers that arrive while a refresh is in flight wait for that refresh instead of
// starting another or being told the key is unknown.
func TestConcurrentCallersShareOneRefresh(t *testing.T) {
	r := newRig(t)
	rotated := r.idp.newKey("kid-2", true)
	token := sign(t, rotated, claimsFor(r.idp, r.clock))
	r.idp.jwksDelay.Store(int64(200 * time.Millisecond))
	before := r.idp.jwksFetches.Load()

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.verify(token)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "every caller gets the rotated key from the one refresh")
	}
	require.Equal(t, int64(1), r.idp.jwksFetches.Load()-before)
}

// A refresh that failed is reported as retryable until the next attempt is allowed,
// never as a permanent "unknown key": clients retry a 503 and give up on a 401.
func TestAFailedRefreshStaysRetryableUntilTheNextAttempt(t *testing.T) {
	r := newRig(t)
	r.idp.jwksDown.Store(true)
	stranger := r.idp.newKey("kid-new", false)

	_, err := r.verify(sign(t, stranger, claimsFor(r.idp, r.clock)))
	require.Equal(t, auth.ReasonKeysUnavailable, reasonOf(t, err))

	for _, kid := range []string{"another-unknown-kid", "yet-another"} {
		stranger.kid = kid
		_, err = r.verify(sign(t, stranger, claimsFor(r.idp, r.clock)))
		require.Equal(t, auth.ReasonKeysUnavailable, reasonOf(t, err), "still the outage, not an unknown key")
	}

	// Once the provider is back and the (short) retry delay has passed, the keys come back.
	r.idp.jwksDown.Store(false)
	rotated := r.idp.newKey("kid-2", true)
	r.clock.Advance(6 * time.Second)
	_, err = r.verify(sign(t, rotated, claimsFor(r.idp, r.clock)))
	require.NoError(t, err, "a failed refresh must not cost the whole 30 second interval")
}

// A clock that jumps back must not freeze refreshing until it catches up.
func TestAClockGoingBackwardsDoesNotFreezeTheRefresh(t *testing.T) {
	r := newRig(t)
	stranger := r.idp.newKey("kid-new", false)
	_, err := r.verify(sign(t, stranger, claimsFor(r.idp, r.clock)))
	require.Equal(t, auth.ReasonUnknownKey, reasonOf(t, err))
	before := r.idp.jwksFetches.Load()

	r.clock.Advance(-time.Hour)
	stranger.kid = "after-the-jump"
	_, err = r.verify(sign(t, stranger, claimsFor(r.idp, r.clock)))
	require.Equal(t, auth.ReasonUnknownKey, reasonOf(t, err))
	require.Equal(t, before+1, r.idp.jwksFetches.Load(), "a look at the provider is allowed again")
}

// One entry in the published keys that this service cannot read must not take the
// others down with it, neither at startup nor on a refresh.
func TestAnUnreadableEntryInTheKeysDoesNotDiscardTheRest(t *testing.T) {
	idp := newFakeIDP(t)
	good := idp.newKey("kid-1", true)
	clk := newClock()

	// Take the good key's public part from what the fake publishes, and add entries
	// of kinds this service does not support next to it.
	resp, err := http.Get(idp.IssuerURL() + "/certs") //nolint:noctx // a test fixture request
	require.NoError(t, err)
	published, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(published, &set))
	set.Keys = append(set.Keys,
		json.RawMessage(`{"kty":"OKP","crv":"X25519","kid":"odd","x":"AAAA"}`),
		json.RawMessage(`{"kty":"bogus","kid":"weird"}`),
		json.RawMessage(`"not even an object"`),
	)
	mixed, err := json.Marshal(set)
	require.NoError(t, err)
	raw := string(mixed)
	idp.rawJWKS.Store(&raw)

	v, err := auth.New(auth.Options{Issuer: idp.IssuerURL(), DiscoveryURL: idp.IssuerURL(), Audience: testAudience, Now: clk.Now})
	require.NoError(t, err)
	defer v.Close()
	require.NoError(t, v.Start(t.Context()))

	_, err = v.Verify(t.Context(), sign(t, good, claimsFor(idp, clk)))
	require.NoError(t, err)
}

func TestAProviderThatPublishesNoUsableKeyIsRefusedAtStart(t *testing.T) {
	idp := newFakeIDP(t)
	empty := `{"keys":[{"kty":"bogus","kid":"x"}]}`
	idp.rawJWKS.Store(&empty)
	v, err := auth.New(auth.Options{Issuer: idp.IssuerURL(), DiscoveryURL: idp.IssuerURL(), Audience: testAudience})
	require.NoError(t, err)
	defer v.Close()
	require.ErrorContains(t, v.Start(t.Context()), "no usable signing keys")
}

// A key too small to be trusted is ignored even if the provider publishes it.
func TestWeakRSAKeysAreIgnored(t *testing.T) {
	r := newRig(t)
	weak := r.idp.newKeyOfSize("kid-weak", 1024, true)
	_, err := r.verify(sign(t, weak, claimsFor(r.idp, r.clock)))
	require.Equal(t, auth.ReasonUnknownKey, reasonOf(t, err))
}

// --- token shapes that should never be accepted ----------------------------------

func TestOtherJWSSerializationsAreRefused(t *testing.T) {
	r := newRig(t)
	token := sign(t, r.key, claimsFor(r.idp, r.clock))
	parts := strings.Split(token, ".")

	flattened := `{"protected":"` + parts[0] + `","payload":"` + parts[1] + `","signature":"` + parts[2] + `"}`
	full := `{"payload":"` + parts[1] + `","signatures":[{"protected":"` + parts[0] + `","signature":"` + parts[2] + `"}]}`
	for name, candidate := range map[string]string{"flattened JSON": flattened, "full JSON": full} {
		t.Run(name, func(t *testing.T) {
			_, err := r.verify(candidate)
			require.Equal(t, auth.ReasonMalformed, reasonOf(t, err))
		})
	}
}

func TestACriticalHeaderIsRefused(t *testing.T) {
	r := newRig(t)
	token := sign(t, r.key, claimsFor(r.idp, r.clock))
	parts := strings.Split(token, ".")
	header := b64(`{"alg":"RS256","typ":"JWT","kid":"kid-1","crit":["exp"],"exp":1}`)
	_, err := r.verify(header + "." + parts[1] + "." + parts[2])
	require.Error(t, err)
	reasonOf(t, err)
}

func TestOnlyAccessTokensAreAccepted(t *testing.T) {
	r := newRig(t)
	good := claimsFor(r.idp, r.clock)

	// A refresh or ID token is not a credential for the API, whatever it is signed with.
	_, err := r.verify(signTyped(t, r.key, good, "refresh+jwt"))
	require.Equal(t, auth.ReasonMalformed, reasonOf(t, err))
	_, err = r.verify(sign(t, r.key, with(good, "typ", "Refresh")))
	require.Equal(t, auth.ReasonInvalidClaims, reasonOf(t, err))
	_, err = r.verify(sign(t, r.key, with(good, "typ", "ID")))
	require.Equal(t, auth.ReasonInvalidClaims, reasonOf(t, err))

	for _, ok := range []string{"JWT", "at+jwt", "AT+JWT"} {
		_, err := r.verify(signTyped(t, r.key, good, ok))
		require.NoError(t, err, "header typ %s", ok)
	}
	_, err = r.verify(sign(t, r.key, with(good, "typ", "Bearer")))
	require.NoError(t, err, "Keycloak marks access tokens typ=Bearer")
}

// --- the principal ------------------------------------------------------------------

func TestOnlyAProviderCarriesAProviderID(t *testing.T) {
	p := auth.NewPrincipal("s", "c", "provider-a", auth.RoleInternal)
	require.Empty(t, p.ProviderID(), "a provider id means something only for the provider role")

	q := auth.NewPrincipal("s", "c", "provider-a", auth.RoleProvider, auth.RoleInternal)
	require.Equal(t, "provider-a", q.ProviderID())

	// End to end: a token with only the internal role does not carry the claim through.
	r := newRig(t)
	claims := with(claimsFor(r.idp, r.clock), "azp", "internal-service")
	claims["resource_access"] = map[string]any{testAudience: map[string]any{"roles": []string{"internal"}}}
	got, err := r.verify(sign(t, r.key, claims))
	require.NoError(t, err)
	require.Empty(t, got.ProviderID())
}

// --- start-up ----------------------------------------------------------------------

func TestAMisconfiguredProviderIsAPermanentError(t *testing.T) {
	r := newRig(t)
	r.idp.issuerOverride = "http://somewhere-else.example/realms/test"
	v, err := auth.New(auth.Options{Issuer: r.idp.IssuerURL(), DiscoveryURL: r.idp.IssuerURL(), Audience: testAudience})
	require.NoError(t, err)
	defer v.Close()
	require.ErrorIs(t, v.Start(t.Context()), auth.ErrMisconfigured, "retrying cannot fix a wrong issuer")

	// An unreachable provider is not: it may simply not be up yet.
	down, err := auth.New(auth.Options{Issuer: "http://127.0.0.1:1/realms/x", DiscoveryURL: "http://127.0.0.1:1/realms/x", Audience: testAudience})
	require.NoError(t, err)
	defer down.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err = down.Start(ctx)
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrMisconfigured)
}

func TestTheErrorChainNeverCarriesTokenContent(t *testing.T) {
	r := newRig(t)
	marker := "SECRET-MARKER-" + strings.Repeat("q", 10)
	good := with(claimsFor(r.idp, r.clock), "note", marker)
	for _, candidate := range []string{
		sign(t, r.key, with(good, "aud", "x")),
		sign(t, r.idp.newKey("kid-1", false), good),
		sign(t, r.key, with(good, "exp", nil)),
		marker,
	} {
		_, err := r.verify(candidate)
		for e := err; e != nil; e = errors.Unwrap(e) {
			require.NotContains(t, e.Error(), marker, "an error in the chain leaks the token")
		}
	}
}
