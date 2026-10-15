//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/httpapi"
)

const (
	providerASecret = "provider-a-local-secret"
	providerBSecret = "provider-b-local-secret"
	internalSecret  = "internal-service-local-secret"
)

// realVerifier validates against the real Keycloak. Options may adjust the
// defaults (audience, clock).
func realVerifier(t *testing.T, adjust func(*auth.Options)) *auth.Verifier {
	t.Helper()
	opts := auth.Options{Issuer: kc.IssuerURL(), DiscoveryURL: kc.IssuerURL(), Audience: "wallet-api"}
	if adjust != nil {
		adjust(&opts)
	}
	v, err := auth.New(opts)
	require.NoError(t, err)
	t.Cleanup(v.Close)
	require.NoError(t, v.Start(t.Context()))
	return v
}

func token(t *testing.T, client, secret string) string {
	t.Helper()
	raw, err := kc.Token(t.Context(), client, secret)
	require.NoError(t, err)
	return raw
}

func reason(t *testing.T, err error) auth.Reason {
	t.Helper()
	var authErr *auth.Error
	require.ErrorAs(t, err, &authErr)
	return authErr.Reason
}

func TestRealTokensYieldTheExpectedPrincipals(t *testing.T) {
	v := realVerifier(t, nil)

	a, err := v.Verify(t.Context(), token(t, "provider-a", providerASecret))
	require.NoError(t, err)
	require.Equal(t, "provider-a", a.ProviderID())
	require.Equal(t, []auth.Role{auth.RoleProvider}, a.Roles())

	b, err := v.Verify(t.Context(), token(t, "provider-b", providerBSecret))
	require.NoError(t, err)
	require.Equal(t, "provider-b", b.ProviderID())
	require.Equal(t, []auth.Role{auth.RoleProvider}, b.Roles())

	internalPrincipal, err := v.Verify(t.Context(), token(t, "internal-service", internalSecret))
	require.NoError(t, err)
	require.Empty(t, internalPrincipal.ProviderID())
	require.Equal(t, []auth.Role{auth.RoleInternal}, internalPrincipal.Roles())
}

func TestARealTokenExpiresWhenTheClockMovesOn(t *testing.T) {
	raw := token(t, "provider-a", providerASecret)
	now := time.Now()

	fresh := realVerifier(t, func(o *auth.Options) { o.Now = func() time.Time { return now } })
	_, err := fresh.Verify(t.Context(), raw)
	require.NoError(t, err)

	// The realm's tokens live five minutes; ten minutes on, the same token is expired.
	later := realVerifier(t, func(o *auth.Options) { o.Now = func() time.Time { return now.Add(10 * time.Minute) } })
	_, err = later.Verify(t.Context(), raw)
	require.Equal(t, auth.ReasonExpired, reason(t, err))
}

func TestARealTokenForAnotherAudienceOrIssuerIsRefused(t *testing.T) {
	raw := token(t, "provider-a", providerASecret)

	otherAudience := realVerifier(t, func(o *auth.Options) { o.Audience = "some-other-api" })
	_, err := otherAudience.Verify(t.Context(), raw)
	require.Equal(t, auth.ReasonWrongAudience, reason(t, err))

	// The same realm reached under another host name announces another issuer.
	require.NotEqual(t, kc.IssuerURL(), kc.AlternateIssuerURL(), "the test needs two distinct issuers")
	fromElsewhere, err := kc.TokenAt(t.Context(), kc.AlternateIssuerURL(), "provider-a", providerASecret)
	require.NoError(t, err)
	_, err = realVerifier(t, nil).Verify(t.Context(), fromElsewhere)
	require.Equal(t, auth.ReasonWrongIssuer, reason(t, err))
}

func TestARealTokenWithATamperedPayloadOrSignatureIsRefused(t *testing.T) {
	v := realVerifier(t, nil)
	raw := token(t, "provider-a", providerASecret)
	parts := strings.Split(raw, ".")
	require.Len(t, parts, 3)

	// Another provider's identity in the payload, the original signature.
	forged := `{"iss":"` + kc.IssuerURL() + `","aud":"wallet-api","exp":99999999999,"azp":"provider-a","provider_id":"provider-b",` +
		`"resource_access":{"wallet-api":{"roles":["provider","internal"]}}}`
	_, err := v.Verify(t.Context(), parts[0]+"."+base64.RawURLEncoding.EncodeToString([]byte(forged))+"."+parts[2])
	require.Equal(t, auth.ReasonBadSignature, reason(t, err))

	// The right payload, a corrupted signature.
	_, err = v.Verify(t.Context(), parts[0]+"."+parts[1]+"."+flipFirstChar(parts[2]))
	require.Equal(t, auth.ReasonBadSignature, reason(t, err))
}

func flipFirstChar(s string) string {
	if s[0] == 'A' {
		return "B" + s[1:]
	}
	return "A" + s[1:]
}

func TestAnUnsignedTokenIsRefusedByTheRealConfiguration(t *testing.T) {
	v := realVerifier(t, nil)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + kc.IssuerURL() + `","aud":"wallet-api","exp":99999999999}`))
	_, err := v.Verify(t.Context(), header+"."+payload+".")
	require.Equal(t, auth.ReasonUnsupportedAlgorithm, reason(t, err))
}

// The same decisions, end to end through the HTTP layer with real tokens.
func TestRealTokensThroughTheHTTPLayer(t *testing.T) {
	v := realVerifier(t, nil)
	logs := &bytes.Buffer{}
	routes := httpapi.NewRoutes(v, slog.New(slog.NewJSONHandler(logs, nil)), prometheus.NewRegistry())
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			http.Error(w, "no principal", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(p.ClientID() + "/" + p.ProviderID()))
	})
	routes.Protected("GET /for-internal", handler, auth.RoleInternal)
	routes.Protected("GET /for-providers", handler, auth.RoleProvider)

	call := func(path, header string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		routes.Handler().ServeHTTP(rec, req)
		return rec
	}
	providerToken := token(t, "provider-a", providerASecret)
	internalToken := token(t, "internal-service", internalSecret)

	rec := call("/for-providers", "Bearer "+providerToken)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "provider-a/provider-a", rec.Body.String(), "the handler sees the identity from the token")

	rec = call("/for-internal", "Bearer "+internalToken)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Equal(t, http.StatusForbidden, call("/for-internal", "Bearer "+providerToken).Code, "a provider cannot reach internal routes")
	require.Equal(t, http.StatusForbidden, call("/for-providers", "Bearer "+internalToken).Code)
	require.Equal(t, http.StatusUnauthorized, call("/for-internal", "").Code)
	require.Equal(t, http.StatusUnauthorized, call("/for-internal", "Bearer not-a-token").Code)
	require.Equal(t, http.StatusUnauthorized, call("/for-internal", "Basic "+providerToken).Code)

	require.NotContains(t, logs.String(), providerToken, "no token reaches the logs")
	require.NotContains(t, logs.String(), internalToken)
}

// A token signed by a key the real Keycloak does not publish: the verifier looks
// at the provider's keys once, finds nothing, and refuses.
func TestATokenSignedByAKeyKeycloakDoesNotPublishIsRefused(t *testing.T) {
	v := realVerifier(t, nil)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT","kid":"not-a-keycloak-key"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + kc.IssuerURL() + `","aud":"wallet-api","exp":99999999999}`))
	_, err := v.Verify(t.Context(), header+"."+payload+"."+base64.RawURLEncoding.EncodeToString([]byte("signature")))
	require.Equal(t, auth.ReasonUnknownKey, reason(t, err))
}

func TestAnUnreachableProviderMakesStartFail(t *testing.T) {
	v, err := auth.New(auth.Options{Issuer: "http://127.0.0.1:1/realms/none", DiscoveryURL: "http://127.0.0.1:1/realms/none", Audience: "wallet-api"})
	require.NoError(t, err)
	defer v.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err = v.Start(ctx)
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
}
