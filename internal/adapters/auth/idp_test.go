package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

const (
	testAudience = "wallet-api"
	testRealm    = "/realms/test"
)

// clock is a settable time source, so tests can move time without sleeping.
type clock struct{ now atomic.Pointer[time.Time] }

func newClock() *clock {
	c := &clock{}
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c.now.Store(&start)
	return c
}

func (c *clock) Now() time.Time { return *c.now.Load() }

func (c *clock) Advance(d time.Duration) {
	next := c.Now().Add(d)
	c.now.Store(&next)
}

type keyPair struct {
	kid string
	key *rsa.PrivateKey
}

// fakeIDP stands in for the identity provider's two endpoints that matter here:
// the discovery document and the signing keys. It signs the tokens the tests
// present. The real Keycloak is exercised by the integration tests.
type fakeIDP struct {
	t      *testing.T
	server *httptest.Server

	mu   sync.Mutex
	keys []keyPair

	issuerOverride string // when set, the discovery document announces this issuer
	jwksDown       atomic.Bool
	jwksFetches    atomic.Int64 // requests received
	jwksServed     atomic.Int64 // requests answered (after any delay)
	jwksDelay      atomic.Int64 // nanoseconds to wait before answering
	rawJWKS        atomic.Pointer[string]
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	f := &fakeIDP{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+testRealm+"/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		issuer := f.IssuerURL()
		if f.issuerOverride != "" {
			issuer = f.issuerOverride
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": f.server.URL + testRealm + "/certs"})
	})
	mux.HandleFunc("GET "+testRealm+"/certs", func(w http.ResponseWriter, req *http.Request) {
		f.jwksFetches.Add(1)
		defer f.jwksServed.Add(1)
		if d := time.Duration(f.jwksDelay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-req.Context().Done():
				return
			}
		}
		if f.jwksDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if raw := f.rawJWKS.Load(); raw != nil {
			_, _ = io.WriteString(w, *raw)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		set := jose.JSONWebKeySet{}
		for _, kp := range f.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: &kp.key.PublicKey, KeyID: kp.kid, Algorithm: "RS256", Use: "sig"})
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIDP) IssuerURL() string { return f.server.URL + testRealm }

// newKey creates a signing key. It is published only if publish is true.
func (f *fakeIDP) newKey(kid string, publish bool) keyPair {
	f.t.Helper()
	return f.newKeyOfSize(kid, 2048, publish)
}

func (f *fakeIDP) newKeyOfSize(kid string, bits int, publish bool) keyPair {
	f.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	require.NoError(f.t, err)
	kp := keyPair{kid: kid, key: key}
	if publish {
		f.publish(kp)
	}
	return kp
}

func (f *fakeIDP) publish(kp keyPair) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, kp)
}

func (f *fakeIDP) unpublish(kid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.keys[:0]
	for _, kp := range f.keys {
		if kp.kid != kid {
			kept = append(kept, kp)
		}
	}
	f.keys = kept
}

// sign produces a signed token with the given claims.
func sign(t *testing.T, kp keyPair, claims map[string]any) string {
	t.Helper()
	return signTyped(t, kp, claims, "JWT")
}

// signTyped signs with a chosen `typ` header.
func signTyped(t *testing.T, kp keyPair, claims map[string]any, typ string) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: kp.key, KeyID: kp.kid}},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)),
	)
	require.NoError(t, err)
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return raw
}

// claimsFor returns the claims of a well-formed provider token at the clock's time.
func claimsFor(f *fakeIDP, c *clock) map[string]any {
	return map[string]any{
		"iss":         f.IssuerURL(),
		"sub":         "service-account-provider-a",
		"aud":         testAudience,
		"azp":         "provider-a",
		"iat":         c.Now().Unix(),
		"exp":         c.Now().Add(5 * time.Minute).Unix(),
		"provider_id": "provider-a",
		"resource_access": map[string]any{
			testAudience: map[string]any{"roles": []string{"provider"}},
		},
	}
}

// with returns a copy of the claims with a field replaced (or removed when nil).
func with(claims map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(claims))
	for k, v := range claims {
		out[k] = v
	}
	if value == nil {
		delete(out, key)
	} else {
		out[key] = value
	}
	return out
}
