package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	defaultRefreshInterval = 30 * time.Second
	defaultMaxKeyAge       = time.Hour
	defaultLeeway          = 30 * time.Second
	httpTimeout            = 5 * time.Second
)

// A provider id is a short, lower-case identifier; anything else in the claim is
// a misconfigured client, not something to pass on to the rest of the system.
var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// signingAlgorithms is every algorithm accepted. Only RS256: it refuses
// `alg: none` and the HMAC algorithms, which an attacker could use with the
// provider's public key as the secret.
var signingAlgorithms = []jose.SignatureAlgorithm{jose.RS256}

// Options configures a Verifier.
type Options struct {
	// Issuer is the `iss` tokens must carry (the URL the provider announces).
	Issuer string
	// DiscoveryURL is where the provider's configuration is fetched from. It
	// differs from Issuer inside Docker, where the provider is reached by its
	// service name but announces the host-facing URL.
	DiscoveryURL string
	// Audience is the resource the tokens must be issued for; the roles are read
	// from the client roles of that same resource.
	Audience string

	// Optional. HTTPClient defaults to a client with its own transport (closed by
	// Close); Now to time.Now; RefreshInterval to 30s (the minimum time between
	// looks at the provider's keys triggered by unknown key ids); MaxKeyAge to 1h;
	// Leeway to 30s (clock skew tolerated on exp, nbf and iat).
	HTTPClient      *http.Client
	Now             func() time.Time
	RefreshInterval time.Duration
	MaxKeyAge       time.Duration
	Leeway          time.Duration
	// Logger receives the cause of key refresh failures, which are otherwise only
	// visible to callers as a bare 503. Defaults to discarding.
	Logger *slog.Logger
}

// Verifier validates access tokens against the identity provider's published
// keys, locally: the provider is contacted only to discover its configuration and
// to refresh its keys, never once per request.
type Verifier struct {
	opts      Options
	transport *http.Transport // non-nil when this Verifier created its own client
	keys      atomic.Pointer[keySet]
}

// New checks the options. It does not touch the network: call Start.
func New(o Options) (*Verifier, error) {
	for name, value := range map[string]string{"issuer": o.Issuer, "discovery URL": o.DiscoveryURL} {
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("auth: the %s must be an absolute http(s) URL, got %q", name, value)
		}
	}
	if o.Audience == "" {
		return nil, errors.New("auth: the audience is required")
	}

	v := &Verifier{opts: o}
	if o.HTTPClient == nil {
		v.transport = http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's own default
		v.opts.HTTPClient = &http.Client{Transport: v.transport, Timeout: httpTimeout}
	}
	if v.opts.Now == nil {
		v.opts.Now = time.Now
	}
	if v.opts.RefreshInterval == 0 {
		v.opts.RefreshInterval = defaultRefreshInterval
	}
	if v.opts.MaxKeyAge == 0 {
		v.opts.MaxKeyAge = defaultMaxKeyAge
	}
	if v.opts.Leeway == 0 {
		v.opts.Leeway = defaultLeeway
	}
	if v.opts.Logger == nil {
		v.opts.Logger = slog.New(slog.DiscardHandler)
	}
	return v, nil
}

// Start discovers the provider's configuration and fetches its keys. It fails if
// the provider announces a different issuer than the one expected, which would
// mean a misconfigured URL (the error then wraps ErrMisconfigured, which no retry
// can fix), and if no key can be had: a service that cannot validate tokens must
// not start.
func (v *Verifier) Start(ctx context.Context) error {
	endpoint := strings.TrimRight(v.opts.DiscoveryURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("auth: discovery: %w", err)
	}
	resp, err := v.opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("auth: discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: discovery: the provider answered %d", resp.StatusCode)
	}

	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return fmt.Errorf("auth: discovery: %w", err)
	}
	if doc.Issuer != v.opts.Issuer {
		return fmt.Errorf("%w: it announces issuer %q, expected %q", ErrMisconfigured, doc.Issuer, v.opts.Issuer)
	}
	if u, err := url.Parse(doc.JWKSURI); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: it announced an unusable jwks_uri %q", ErrMisconfigured, doc.JWKSURI)
	}

	ks := newKeySet(doc.JWKSURI, v.opts.HTTPClient, v.opts.Now, v.opts.Logger, v.opts.RefreshInterval, v.opts.MaxKeyAge)
	if err := ks.load(ctx); err != nil {
		ks.close()
		return fmt.Errorf("auth: %w", err)
	}
	if old := v.keys.Swap(ks); old != nil {
		old.close()
	}
	return nil
}

// Close stops the background key refreshes and releases the idle connections of
// the verifier's own HTTP client, so nothing outlives the application.
func (v *Verifier) Close() {
	if ks := v.keys.Load(); ks != nil {
		ks.close()
	}
	if v.transport != nil {
		v.transport.CloseIdleConnections()
	}
}

// extraClaims are the claims, beyond the registered ones, the service reads.
type extraClaims struct {
	Type            string `json:"typ"`
	AuthorizedParty string `json:"azp"`
	ProviderID      string `json:"provider_id"`
	ResourceAccess  map[string]struct {
		Roles []string `json:"roles"`
	} `json:"resource_access"`
}

// Verify checks the token (signature, algorithm, issuer, audience, expiry) and
// returns who it belongs to. Every failure is an *Error with a Reason.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	keys := v.keys.Load()
	if keys == nil {
		return Principal{}, fail(ReasonKeysUnavailable, errors.New("the verifier was not started"))
	}
	if raw == "" {
		return Principal{}, fail(ReasonMalformed, nil)
	}

	token, err := jwt.ParseSigned(raw, signingAlgorithms)
	if err != nil {
		var unexpected *jose.ErrUnexpectedSignatureAlgorithm
		if errors.As(err, &unexpected) {
			return Principal{}, fail(ReasonUnsupportedAlgorithm, err)
		}
		return Principal{}, fail(ReasonMalformed, err)
	}
	if len(token.Headers) != 1 || token.Headers[0].KeyID == "" {
		return Principal{}, fail(ReasonMalformed, errors.New("the token must have one signature and a key id"))
	}
	if err := checkHeader(token.Headers[0]); err != nil {
		return Principal{}, err
	}

	key, kerr := keys.key(ctx, token.Headers[0].KeyID)
	if kerr != nil {
		return Principal{}, kerr
	}
	if key.Algorithm != "" && key.Algorithm != string(jose.RS256) {
		return Principal{}, fail(ReasonBadSignature, errors.New("the key is not for RS256"))
	}

	var registered jwt.Claims
	var extra extraClaims
	// Claims verifies the signature before it decodes anything.
	if err := token.Claims(key.Key, &registered, &extra); err != nil {
		if errors.Is(err, jose.ErrCryptoFailure) {
			return Principal{}, fail(ReasonBadSignature, err)
		}
		return Principal{}, fail(ReasonMalformed, err)
	}

	// Refresh and ID tokens are signed by the same keys; they are not credentials
	// for this API. Keycloak marks its access tokens `Bearer`.
	if extra.Type != "" && !strings.EqualFold(extra.Type, "Bearer") {
		return Principal{}, fail(ReasonInvalidClaims, errors.New("the token is not an access token"))
	}

	// A token with no expiry would never expire, and the validation below only
	// checks an expiry that is present.
	if registered.Expiry == nil {
		return Principal{}, fail(ReasonInvalidClaims, errors.New("the token has no expiry"))
	}
	err = registered.ValidateWithLeeway(jwt.Expected{
		Issuer:      v.opts.Issuer,
		AnyAudience: jwt.Audience{v.opts.Audience},
		Time:        v.opts.Now(),
	}, v.opts.Leeway)
	switch {
	case err == nil:
	case errors.Is(err, jwt.ErrInvalidIssuer):
		return Principal{}, fail(ReasonWrongIssuer, err)
	case errors.Is(err, jwt.ErrInvalidAudience):
		return Principal{}, fail(ReasonWrongAudience, err)
	case errors.Is(err, jwt.ErrExpired):
		return Principal{}, fail(ReasonExpired, err)
	case errors.Is(err, jwt.ErrNotValidYet), errors.Is(err, jwt.ErrIssuedInTheFuture):
		return Principal{}, fail(ReasonNotYetValid, err)
	default:
		return Principal{}, fail(ReasonInvalidClaims, err)
	}

	roles := make([]Role, 0, len(extra.ResourceAccess[v.opts.Audience].Roles))
	for _, name := range extra.ResourceAccess[v.opts.Audience].Roles {
		roles = append(roles, Role(name))
	}
	principal := NewPrincipal(registered.Subject, extra.AuthorizedParty, extra.ProviderID, roles...)

	if extra.ProviderID != "" && !providerIDPattern.MatchString(extra.ProviderID) {
		return Principal{}, fail(ReasonInvalidClaims, errors.New("the provider id has an invalid shape"))
	}
	// A provider that does not say which provider it is cannot be allowed to touch anything.
	if principal.HasRole(RoleProvider) && principal.ProviderID() == "" {
		return Principal{}, fail(ReasonInvalidClaims, errors.New("the provider role requires a provider id"))
	}
	return principal, nil
}

// checkHeader refuses tokens whose header marks them as something other than an
// access token. (The JSON serializations and `crit` headers are refused by the
// parser itself; the tests pin that.)
func checkHeader(h jose.Header) *Error {
	if typ, ok := h.ExtraHeaders[jose.HeaderType].(string); ok &&
		!strings.EqualFold(typ, "JWT") && !strings.EqualFold(typ, "at+jwt") {
		return fail(ReasonMalformed, errors.New("the token is not an access token"))
	}
	return nil
}
