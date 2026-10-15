package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const swaggerOrigin = "http://localhost:8089"

func (e *walletEnv) withCORS(origins ...string) *walletEnv {
	e.routes.AllowOrigins(origins)
	return e
}

func (e *walletEnv) request(method, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(""))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.routes.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWithoutConfiguredOriginsNoCORSHeadersAreEverSent(t *testing.T) {
	e := newWalletEnv(t)
	rec := e.request(http.MethodGet, "/wallets/"+uuid.NewString(), "Origin", swaggerOrigin, "Authorization", "Bearer internal-token")
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Empty(t, rec.Header().Get("Vary"))

	rec = e.request(http.MethodOptions, "/wallets", "Origin", swaggerOrigin, "Access-Control-Request-Method", "POST")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code, "the preflight is not answered")
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestAnAllowedOriginReceivesTheHeadersOnEveryAnswer(t *testing.T) {
	e := newWalletEnv(t).withCORS(swaggerOrigin)
	for name, headers := range map[string][]string{
		"success":         {"Origin", swaggerOrigin, "Authorization", "Bearer internal-token"},
		"unauthenticated": {"Origin", swaggerOrigin},
		"forbidden":       {"Origin", swaggerOrigin, "Authorization", "Bearer provider-token"},
	} {
		rec := e.request(http.MethodGet, "/wallets/"+uuid.NewString(), headers...)
		require.Equal(t, swaggerOrigin, rec.Header().Get("Access-Control-Allow-Origin"), name)
		require.Contains(t, rec.Header().Values("Vary"), "Origin", name)
		require.Contains(t, rec.Header().Get("Access-Control-Expose-Headers"), "X-Correlation-Id", name)
		require.NotEqual(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestAPreflightFromAnAllowedOriginIsAnsweredWithoutCredentialsAndReachesNothing(t *testing.T) {
	e := newWalletEnv(t).withCORS(swaggerOrigin)
	rec := e.request(http.MethodOptions, "/wallets",
		"Origin", swaggerOrigin, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization,content-type")

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, swaggerOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, "GET, POST, OPTIONS", rec.Header().Get("Access-Control-Allow-Methods"))
	for _, header := range []string{"Authorization", "Content-Type", "X-Correlation-Id"} {
		require.Contains(t, rec.Header().Get("Access-Control-Allow-Headers"), header)
	}
	require.Equal(t, "600", rec.Header().Get("Access-Control-Max-Age"))
	require.Empty(t, e.opener.calls)
	require.Empty(t, e.reader.calls)
}

func TestAnOriginThatIsNotAllowedGetsNothing(t *testing.T) {
	e := newWalletEnv(t).withCORS(swaggerOrigin)
	for _, origin := range []string{"http://evil.example", "http://localhost:8090", "https://localhost:8089", "null", swaggerOrigin + ".evil.example"} {
		rec := e.request(http.MethodGet, "/wallets/"+uuid.NewString(), "Origin", origin, "Authorization", "Bearer internal-token")
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), origin)

		rec = e.request(http.MethodOptions, "/wallets", "Origin", origin, "Access-Control-Request-Method", "POST")
		require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), origin)
		require.NotEqual(t, http.StatusNoContent, rec.Code, "the preflight of %s is not approved", origin)
	}
}

func TestARequestWithoutOriginIsUntouchedExceptForVary(t *testing.T) {
	e := newWalletEnv(t).withCORS(swaggerOrigin)
	rec := e.request(http.MethodGet, "/wallets/"+uuid.NewString(), "Authorization", "Bearer internal-token")
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, rec.Header().Values("Vary"), "Origin")
}

func TestSeveralOriginsAreEachMatchedExactly(t *testing.T) {
	e := newWalletEnv(t).withCORS(swaggerOrigin, "https://docs.example.com")
	for _, origin := range []string{swaggerOrigin, "https://docs.example.com"} {
		rec := e.request(http.MethodGet, "/wallets/"+uuid.NewString(), "Origin", origin, "Authorization", "Bearer internal-token")
		require.Equal(t, origin, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}
