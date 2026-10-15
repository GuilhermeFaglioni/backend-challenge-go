package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/GuilhermeFaglioni/backend-challenge-go/api"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/health"
)

// maxTokenBytes caps the credential the service is willing to parse.
const maxTokenBytes = 8 << 10

// retryAfterSeconds is what clients are told to wait when authentication is
// temporarily unavailable.
const retryAfterSeconds = "5"

// publicRoutes is the allowlist of routes that need no credentials. Adding a
// route here is how a route becomes public, and it is a decision to review: any
// other route is registered with Protected and must name its roles.
var publicRoutes = map[string]bool{
	"GET /health/live":  true,
	"GET /health/ready": true,
	"GET /metrics":      true,
	"GET /openapi.yaml": true,
}

// Authenticator turns a bearer token into the caller it belongs to.
type Authenticator interface {
	Verify(ctx context.Context, token string) (auth.Principal, error)
}

// Route describes a registered route, for the guard tests and for documentation.
type Route struct {
	Pattern string
	Public  bool
	Roles   []auth.Role // the roles allowed, when not public
}

// Routes is the only way a route reaches the HTTP server: it owns the router.
// Every route is either on the public allowlist or protected by a role it
// declares, so a route cannot be added without a decision about who may call it.
// A test fails the build if any other file uses a router directly.
//
// Registering a route wrongly (not on the allowlist, no roles, an unknown role)
// panics: it is a programming error found when the application is composed, and
// the test that builds the real route table runs it on every build.
type Routes struct {
	mux        *http.ServeMux
	authn      Authenticator
	log        *slog.Logger
	failures   *prometheus.CounterVec
	registered []Route
	cors       corsPolicy
}

// allReasons is every reason an authentication outcome is counted under, so that
// every series exists from the start and a dashboard can tell "none yet" from
// "not measured".
var allReasons = []auth.Reason{
	auth.ReasonMissing, auth.ReasonMalformed, auth.ReasonUnsupportedAlgorithm, auth.ReasonUnknownKey,
	auth.ReasonBadSignature, auth.ReasonExpired, auth.ReasonNotYetValid, auth.ReasonWrongIssuer,
	auth.ReasonWrongAudience, auth.ReasonInvalidClaims, auth.ReasonKeysUnavailable, auth.ReasonForbidden,
}

// NewRoutes creates an empty route table. Authentication failures are counted
// in auth_failures_total{reason}.
func NewRoutes(authn Authenticator, log *slog.Logger, reg prometheus.Registerer) *Routes {
	failures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "auth_failures_total",
		Help: "Requests refused by authentication or authorization, by reason.",
	}, []string{"reason"})
	for _, reason := range allReasons {
		failures.WithLabelValues(string(reason))
	}
	reg.MustRegister(failures)
	return &Routes{mux: http.NewServeMux(), authn: authn, log: log, failures: failures}
}

// Public registers a route that needs no credentials. The pattern must be on the
// allowlist.
func (r *Routes) Public(pattern string, h http.Handler) {
	if !publicRoutes[pattern] {
		panic(fmt.Sprintf("httpapi: %q is not on the allowlist of public routes; register it with Protected and its roles", pattern))
	}
	r.mux.Handle(pattern, h)
	r.registered = append(r.registered, Route{Pattern: pattern, Public: true})
}

// Protected registers a route that requires a valid token whose principal has at
// least one of the roles.
func (r *Routes) Protected(pattern string, h http.Handler, roles ...auth.Role) {
	if h == nil {
		panic(fmt.Sprintf("httpapi: %q has no handler", pattern))
	}
	if len(roles) == 0 {
		panic(fmt.Sprintf("httpapi: %q must declare the roles allowed to call it", pattern))
	}
	for _, role := range roles {
		if !auth.NewPrincipal("", "", "", role).HasRole(role) {
			panic(fmt.Sprintf("httpapi: %q declares the unknown role %q", pattern, role))
		}
	}
	r.mux.Handle(pattern, r.authenticate(roles, h))
	r.registered = append(r.registered, Route{Pattern: pattern, Roles: slices.Clone(roles)})
}

// AllowOrigins lets browsers on the given origins call the API (see corsPolicy).
// Call it before Handler.
func (r *Routes) AllowOrigins(origins []string) { r.cors = corsPolicy{origins: slices.Clone(origins)} }

// Handler is the router to serve.
func (r *Routes) Handler() http.Handler {
	h := http.HandlerFunc(r.mux.ServeHTTP)
	if len(r.cors.origins) == 0 {
		return h
	}
	return r.cors.wrap(h)
}

// Registered lists every route, in registration order.
func (r *Routes) Registered() []Route { return slices.Clone(r.registered) }

// authenticate wraps a handler with the credential and role checks.
func (r *Routes) authenticate(roles []auth.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()

		token, reason := bearerToken(req.Header)
		if reason != "" {
			r.refuse(ctx, w, req, reason)
			return
		}

		principal, err := r.authn.Verify(ctx, token)
		if err != nil {
			var authErr *auth.Error
			if errors.As(err, &authErr) {
				r.refuse(ctx, w, req, authErr.Reason)
				return
			}
			// Not an authentication verdict: fail closed and say it is temporary.
			r.log.ErrorContext(ctx, "authentication could not be completed", slog.Any("error", err))
			r.refuse(ctx, w, req, auth.ReasonKeysUnavailable)
			return
		}

		if !principal.HasAnyRole(roles...) {
			r.refuse(ctx, w, req, auth.ReasonForbidden)
			return
		}
		next.ServeHTTP(w, req.WithContext(auth.WithPrincipal(ctx, principal)))
	})
}

// refuse counts and logs a refusal, then answers. The body is the same for every
// reason of a kind: nothing a caller could use to probe. The credential is never
// logged, counted or echoed.
func (r *Routes) refuse(ctx context.Context, w http.ResponseWriter, req *http.Request, reason auth.Reason) {
	r.failures.WithLabelValues(string(reason)).Inc()
	r.log.InfoContext(ctx, "authentication failed",
		slog.String("reason", string(reason)),
		slog.String("method", req.Method),
		slog.String("path", req.URL.Path),
	)
	switch reason {
	case auth.ReasonForbidden:
		writeError(w, http.StatusForbidden, CodeForbidden, "the credentials do not allow this")
	case auth.ReasonKeysUnavailable:
		w.Header().Set("Retry-After", retryAfterSeconds)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "authentication is temporarily unavailable")
	default:
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, CodeUnauthenticated, "authentication required")
	}
}

// bearerToken extracts the token from `Authorization: Bearer <token>`. It returns
// the reason it cannot, when there is none: a missing header, or one that is not
// a single well-formed bearer credential.
func bearerToken(header http.Header) (token string, reason auth.Reason) {
	values := header.Values("Authorization")
	switch len(values) {
	case 0:
		return "", auth.ReasonMissing
	case 1:
	default:
		return "", auth.ReasonMalformed // two credentials: which one is meant?
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return "", auth.ReasonMissing
	}
	scheme, rest, found := strings.Cut(value, " ")
	rest = strings.TrimSpace(rest)
	if !found || !strings.EqualFold(scheme, "Bearer") || rest == "" || strings.ContainsAny(rest, " \t") || len(rest) > maxTokenBytes {
		return "", auth.ReasonMalformed
	}
	return rest, ""
}

// registerRoutes declares every route of the application. A business endpoint is
// added here with Protected and the roles that may call it.
func registerRoutes(r *Routes, hr *health.Registry, reg *prometheus.Registry, wallets *Wallets) {
	r.Public("GET /health/live", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	r.Public("GET /health/ready", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		report, ok := hr.Ready(req.Context())
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, report)
	}))
	r.Public("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	r.Public("GET /openapi.yaml", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.OpenAPI)
	}))

	wallets.register(r)
}
