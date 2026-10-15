package httpapi

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/api"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

// fakeAuthenticator answers by token, with no cryptography: the verifier has its
// own tests. It lets these tests drive every outcome of the middleware.
type fakeAuthenticator map[string]result

type result struct {
	principal auth.Principal
	reason    auth.Reason // when set, verification fails with it
}

func (f fakeAuthenticator) Verify(_ context.Context, token string) (auth.Principal, error) {
	r, ok := f[token]
	if !ok {
		return auth.Principal{}, &auth.Error{Reason: auth.ReasonMalformed}
	}
	if r.reason != "" {
		return auth.Principal{}, &auth.Error{Reason: r.reason}
	}
	return r.principal, nil
}

var (
	providerA = auth.NewPrincipal("sub-a", "provider-a", "provider-a", auth.RoleProvider)
	providerB = auth.NewPrincipal("sub-b", "provider-b", "provider-b", auth.RoleProvider)
	internal  = auth.NewPrincipal("sub-i", "internal-service", "", auth.RoleInternal)
)

type harness struct {
	authn    fakeAuthenticator
	routes   *Routes
	registry *prometheus.Registry
	logs     *bytes.Buffer
	seen     *auth.Principal // principal the protected handler saw
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{registry: prometheus.NewRegistry(), logs: &bytes.Buffer{}}
	logger := slog.New(slog.NewJSONHandler(h.logs, nil))
	h.authn = fakeAuthenticator{
		"provider-token": {principal: providerA},
		"internal-token": {principal: internal},
		"expired-token":  {reason: auth.ReasonExpired},
		"forged-token":   {reason: auth.ReasonBadSignature},
		"outage-token":   {reason: auth.ReasonKeysUnavailable},
	}
	h.routes = NewRoutes(h.authn, logger, h.registry)

	h.routes.Protected("GET /internal-only", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFrom(r.Context()); ok {
			h.seen = &p
		}
		w.WriteHeader(http.StatusNoContent)
	}), auth.RoleInternal)
	h.routes.Protected("GET /either", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), auth.RoleInternal, auth.RoleProvider)
	h.routes.Public("GET /health/live", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	return h
}

func (h *harness) do(method, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.routes.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *harness) failures(t *testing.T, reason auth.Reason) float64 {
	t.Helper()
	families, err := h.registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "auth_failures_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelValue(metric, "reason") == string(reason) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

const (
	unauthenticatedBody = `{"error":{"code":"UNAUTHENTICATED","message":"authentication required"}}` + "\n"
	forbiddenBody       = `{"error":{"code":"FORBIDDEN","message":"the credentials do not allow this"}}` + "\n"
	unavailableBody     = `{"error":{"code":"UNAVAILABLE","message":"authentication is temporarily unavailable"}}` + "\n"
)

func TestAValidTokenWithTheRightRoleReachesTheHandlerWithItsPrincipal(t *testing.T) {
	h := newHarness(t)

	rec := h.do(http.MethodGet, "/internal-only", "Authorization", "Bearer internal-token")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, h.seen)
	require.Equal(t, "internal-service", h.seen.ClientID())

	rec = h.do(http.MethodGet, "/either", "Authorization", "Bearer provider-token")
	require.Equal(t, http.StatusNoContent, rec.Code, "any one of the declared roles is enough")
}

func TestTheSchemeIsCaseInsensitiveLikeTheStandardSays(t *testing.T) {
	h := newHarness(t)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		rec := h.do(http.MethodGet, "/internal-only", "Authorization", scheme+" internal-token")
		require.Equal(t, http.StatusNoContent, rec.Code, scheme)
	}
}

func TestUnauthenticatedRequestsAreRefusedWithTheContractsBodyAndNoDetail(t *testing.T) {
	longToken := strings.Repeat("a", maxTokenBytes+1)
	tests := []struct {
		name    string
		headers []string
		reason  auth.Reason
	}{
		{"no header", nil, auth.ReasonMissing},
		{"empty header", []string{"Authorization", ""}, auth.ReasonMissing},
		{"another scheme", []string{"Authorization", "Basic dXNlcjpwYXNz"}, auth.ReasonMalformed},
		{"scheme without a token", []string{"Authorization", "Bearer"}, auth.ReasonMalformed},
		{"scheme and blank token", []string{"Authorization", "Bearer   "}, auth.ReasonMalformed},
		{"a token with a space inside", []string{"Authorization", "Bearer a b"}, auth.ReasonMalformed},
		{"two Authorization headers", []string{"Authorization", "Bearer internal-token", "Authorization", "Bearer internal-token"}, auth.ReasonMalformed},
		{"an oversized token", []string{"Authorization", "Bearer " + longToken}, auth.ReasonMalformed},
		{"a token the verifier refuses as expired", []string{"Authorization", "Bearer expired-token"}, auth.ReasonExpired},
		{"a token with a bad signature", []string{"Authorization", "Bearer forged-token"}, auth.ReasonBadSignature},
		{"a token nobody issued", []string{"Authorization", "Bearer nonsense"}, auth.ReasonMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			// The oversized token would be accepted by the verifier: only the size limit refuses it.
			h.authn[longToken] = result{principal: internal}
			rec := h.do(http.MethodGet, "/internal-only", tt.headers...)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			require.Equal(t, unauthenticatedBody, rec.Body.String(), "the same body whatever the reason: no detail to learn from")
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			require.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"))
			require.Nil(t, h.seen, "the handler must not run")
			require.InDelta(t, 1, h.failures(t, tt.reason), 0, "failure counted under %s", tt.reason)
		})
	}
}

func TestARoleThatIsNotEnoughIsForbiddenNotUnauthenticated(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/internal-only", "Authorization", "Bearer provider-token")

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, forbiddenBody, rec.Body.String())
	require.Empty(t, rec.Header().Get("WWW-Authenticate"), "the credentials were understood")
	require.Nil(t, h.seen)
	require.InDelta(t, 1, h.failures(t, auth.ReasonForbidden), 0)
}

func TestAProviderOutageIsRetryableNotARefusal(t *testing.T) {
	h := newHarness(t)
	rec := h.do(http.MethodGet, "/internal-only", "Authorization", "Bearer outage-token")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, unavailableBody, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.InDelta(t, 1, h.failures(t, auth.ReasonKeysUnavailable), 0)
}

func TestPublicRoutesNeedNoCredentials(t *testing.T) {
	h := newHarness(t)
	require.Equal(t, http.StatusNoContent, h.do(http.MethodGet, "/health/live").Code)
	require.Equal(t, http.StatusNoContent, h.do(http.MethodGet, "/health/live", "Authorization", "Bearer garbage").Code,
		"a public route does not look at the header at all")
}

// A refusal must be visible in the metrics and the logs, but the credential
// itself must not reach either of them, nor the response.
func TestNoTokenOrCredentialEverReachesLogsMetricsOrResponses(t *testing.T) {
	h := newHarness(t)
	secret := "SECRET-TOKEN-DO-NOT-LEAK"

	for _, header := range []string{"Bearer " + secret, "Basic " + secret, secret} {
		rec := h.do(http.MethodGet, "/internal-only", "Authorization", header)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.NotContains(t, rec.Body.String(), secret)
		require.NotContains(t, rec.Header().Get("WWW-Authenticate"), secret)
	}
	require.NotContains(t, h.logs.String(), secret)
	require.Contains(t, h.logs.String(), "authentication failed", "refusals are logged, without the credential")

	families, err := h.registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		require.NotContains(t, family.String(), secret)
	}
}

func TestEveryAuthenticationOutcomeStartsAtZeroInTheMetrics(t *testing.T) {
	h := newHarness(t)
	families, err := h.registry.Gather()
	require.NoError(t, err)
	var reasons []string
	for _, family := range families {
		if family.GetName() == "auth_failures_total" {
			for _, m := range family.GetMetric() {
				reasons = append(reasons, labelValue(m, "reason"))
			}
		}
	}
	for _, want := range []auth.Reason{
		auth.ReasonMissing, auth.ReasonMalformed, auth.ReasonUnsupportedAlgorithm, auth.ReasonUnknownKey,
		auth.ReasonBadSignature, auth.ReasonExpired, auth.ReasonNotYetValid, auth.ReasonWrongIssuer,
		auth.ReasonWrongAudience, auth.ReasonInvalidClaims, auth.ReasonKeysUnavailable, auth.ReasonForbidden, auth.ReasonProviderMismatch,
	} {
		require.Contains(t, reasons, string(want), "the counter has no series for %s until it first happens", want)
	}
}

// --- the registry refuses what would open a hole ----------------------------------

func newRoutes() *Routes {
	return NewRoutes(fakeAuthenticator{}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), prometheus.NewRegistry())
}

func TestARouteCannotBePublicUnlessItIsOnTheAllowlist(t *testing.T) {
	r := newRoutes()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	for _, pattern := range []string{"GET /health/live", "GET /health/ready", "GET /metrics", "GET /openapi.yaml"} {
		require.NotPanics(t, func() { r.Public(pattern, h) }, pattern)
	}
	for _, pattern := range []string{"POST /wallets", "GET /wallets/{id}", "GET /health/secret", "GET /metrics/all", "/health/live"} {
		require.Panics(t, func() { r.Public(pattern, h) }, "%q is not on the allowlist of public routes", pattern)
	}
}

func TestAProtectedRouteMustDeclareAKnownRole(t *testing.T) {
	r := newRoutes()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	require.Panics(t, func() { r.Protected("POST /wallets", h) }, "no role declared")
	require.Panics(t, func() { r.Protected("POST /wallets", h, auth.Role("superuser")) }, "unknown role")
	require.Panics(t, func() { r.Protected("POST /wallets", nil, auth.RoleInternal) }, "no handler")
	require.NotPanics(t, func() { r.Protected("POST /wallets", h, auth.RoleInternal) })
}

func TestTheRegistryListsWhatWasRegistered(t *testing.T) {
	h := newHarness(t)
	require.ElementsMatch(t, []Route{
		{Pattern: "GET /internal-only", Roles: []auth.Role{auth.RoleInternal}},
		{Pattern: "GET /either", Roles: []auth.Role{auth.RoleInternal, auth.RoleProvider}},
		{Pattern: "GET /health/live", Public: true},
	}, h.routes.Registered())
}

// --- the structural guards ---------------------------------------------------------

// Routes registered through the registry are authenticated by construction, so the
// only way to add an unprotected route is to bypass it: build a ServeMux, register
// a handler on the default one, or start another server. This test makes that
// fail the build. The one http.Server literal allowed lives in server.go and must
// serve the registry's handler.
func TestNoRouteBypassesTheRegistry(t *testing.T) {
	forbidden := map[string]bool{
		"NewServeMux": true, "ServeMux": true, "DefaultServeMux": true,
		"Handle": true, "HandleFunc": true,
		"ListenAndServe": true, "ListenAndServeTLS": true,
	}
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || path == "routes.go" {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // the package's own sources
		require.NoError(t, err)
		file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		require.NoError(t, err)
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				pkg, isPkg := node.X.(*ast.Ident)
				onHTTP := isPkg && pkg.Name == "http"
				// srv.Serve(listener) is how server.go starts the one allowed server;
				// the package-level http.Serve would start another.
				serves := (node.Sel.Name == "Serve" || node.Sel.Name == "ServeTLS") && (onHTTP || path != "server.go")
				if forbidden[node.Sel.Name] || serves {
					t.Errorf("%s uses %s: routes must be registered through Routes (routes.go) so that authentication cannot be skipped", path, node.Sel.Name)
				}
			case *ast.CompositeLit:
				sel, ok := node.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Server" {
					return true
				}
				if path != "server.go" {
					t.Errorf("%s builds an http.Server: only server.go may, serving Routes.Handler()", path)
					return true
				}
				if !servesTheRegistry(node) {
					t.Errorf("%s: the http.Server must have Handler: routes.Handler()", path)
				}
			}
			return true
		})
	}
	require.NotZero(t, checked, "no files were checked: the test is not looking in the right place")
}

// servesTheRegistry reports whether the server literal sets Handler to a call of
// a Handler method (routes.Handler()).
func servesTheRegistry(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok || kv.Key.(*ast.Ident).Name != "Handler" {
			continue
		}
		call, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Handler"
	}
	return false
}

// The application's real route table builds. The guarantee that every route is
// public-by-allowlist or protected by a role is the registry's: it panics at
// registration otherwise, which this test (and the whole startup) would trip over.
func TestTheApplicationsRouteTableBuilds(t *testing.T) {
	r := newRoutes()
	require.NotPanics(t, func() {
		registerRoutes(r, health.NewRegistry(health.Params{Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))}), prometheus.NewRegistry(), NewWallets(&fakeOpener{}, &fakeReader{}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))), NewWagering(&fakeProcessor{}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))), NewTransactions(&scopedReader{}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))))
	})
	require.NotEmpty(t, r.Registered())
}

// The OpenAPI document and the routes the application registers describe the same
// API: a route nobody documented, or a documented one that is not served, fails here.
func TestTheRegisteredRoutesAreExactlyTheDocumentedOnes(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	require.NoError(t, err)
	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	r := newRoutes()
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	registerRoutes(r, health.NewRegistry(health.Params{Logger: logger}), prometheus.NewRegistry(), NewWallets(&fakeOpener{}, &fakeReader{}, logger), NewWagering(&fakeProcessor{}, logger), NewTransactions(&scopedReader{}, logger))
	registered := map[string]bool{}
	for _, route := range r.Registered() {
		registered[route.Pattern] = true
	}
	require.Equal(t, documented, registered)
}
