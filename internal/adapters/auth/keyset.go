package auth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	maxJWKSBytes = 1 << 20
	// minRSAKeyBits is the smallest RSA key trusted to sign tokens.
	minRSAKeyBits = 2048
	// failureRetry is how soon another look at the provider is allowed after one
	// failed. It is shorter than the normal interval: a failure is an outage, not
	// proof that the key does not exist, and callers should not wait long for it.
	failureRetry = 5 * time.Second
)

// snapshot is an immutable set of keys, replaced whole by a refresh.
type snapshot struct {
	keys      map[string]jose.JSONWebKey
	fetchedAt time.Time
}

// flight is one refresh in progress. Callers that need its outcome wait on done;
// err is written before done is closed and read only after.
type flight struct {
	done chan struct{}
	err  error
}

// keySet caches the identity provider's public signing keys and looks them up by
// key id.
//
// Lookups of a known key never wait: they read an immutable snapshot. A token
// naming a key id the cache does not have makes the cache look at the provider
// again, but at most once per minRefresh: anyone can send tokens with made-up key
// ids, and each of them must not cost the provider a request. A legitimate
// rotation is picked up by the first token that uses the new key.
//
// A refresh runs on its own goroutine with its own deadline, not on the request
// that triggered it. Otherwise the caller could cancel it, and anyone able to
// cancel a request could use up the interval's one refresh and keep a rotated key
// from ever being picked up. Concurrent callers share the refresh in flight.
//
// Keys older than maxAge are refreshed so a revoked key is not honoured forever;
// if the provider cannot be reached, the last known keys keep working.
type keySet struct {
	jwksURL    string
	client     *http.Client
	now        func() time.Time
	log        *slog.Logger
	minRefresh time.Duration
	maxAge     time.Duration

	snap atomic.Pointer[snapshot]

	base   context.Context //nolint:containedctx // the lifetime of background refreshes, ended by close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex // guards the fields below; never held during I/O
	attemptAt time.Time  // when the last refresh was started because of a lookup; zero if none
	lastErr   error      // outcome of the last refresh: nil when it succeeded
	inflight  *flight
	closed    bool
}

func newKeySet(jwksURL string, client *http.Client, now func() time.Time, log *slog.Logger, minRefresh, maxAge time.Duration) *keySet {
	k := &keySet{jwksURL: jwksURL, client: client, now: now, log: log, minRefresh: minRefresh, maxAge: maxAge}
	k.base, k.cancel = context.WithCancel(context.Background())
	return k
}

// load fetches the keys unconditionally, on the caller's context. The first call
// is made by Verifier.Start and does not count against the refresh interval.
func (k *keySet) load(ctx context.Context) error {
	snap, err := k.fetch(ctx)
	if err != nil {
		return err
	}
	k.snap.Store(snap)
	return nil
}

// close stops background refreshes and waits for them.
func (k *keySet) close() {
	k.mu.Lock()
	k.closed = true
	k.mu.Unlock()
	k.cancel()
	k.wg.Wait()
}

// key returns the key with the given id, or the reason it cannot be had.
func (k *keySet) key(ctx context.Context, kid string) (jose.JSONWebKey, *Error) {
	snap := k.snap.Load()
	key, found := snap.keys[kid]
	if found && k.now().Sub(snap.fetchedAt) <= k.maxAge {
		return key, nil
	}

	// The key is unknown, or the cache is old: a refresh is due, if allowed.
	f := k.startRefresh()
	if f == nil {
		return k.withoutRefresh(key, found)
	}
	select {
	case <-f.done:
	case <-ctx.Done():
		if found {
			return key, nil // stale beats nothing
		}
		return jose.JSONWebKey{}, fail(ReasonKeysUnavailable, ctx.Err())
	}

	if key, found = k.snap.Load().keys[kid]; found {
		return key, nil // fresh, or stale because the refresh failed
	}
	if f.err != nil {
		return jose.JSONWebKey{}, fail(ReasonKeysUnavailable, f.err)
	}
	return jose.JSONWebKey{}, fail(ReasonUnknownKey, nil)
}

// withoutRefresh answers a lookup that may not trigger a refresh right now.
func (k *keySet) withoutRefresh(key jose.JSONWebKey, found bool) (jose.JSONWebKey, *Error) {
	if found {
		return key, nil
	}
	k.mu.Lock()
	lastErr := k.lastErr
	k.mu.Unlock()
	if lastErr != nil {
		// The last look failed, so an unknown key may only be unknown because of
		// the outage: say so, and let the client retry.
		return jose.JSONWebKey{}, fail(ReasonKeysUnavailable, lastErr)
	}
	return jose.JSONWebKey{}, fail(ReasonUnknownKey, nil)
}

// startRefresh returns the refresh in flight, starts one if the interval allows,
// or returns nil.
func (k *keySet) startRefresh() *flight {
	now := k.now()

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return nil
	}
	if k.inflight != nil {
		return k.inflight
	}
	wait := k.minRefresh
	if k.lastErr != nil {
		wait = failureRetry
	}
	// A clock that went backwards (since < 0) must not freeze refreshing.
	if since := now.Sub(k.attemptAt); !k.attemptAt.IsZero() && since >= 0 && since < wait {
		return nil
	}

	k.attemptAt = now
	f := &flight{done: make(chan struct{})}
	k.inflight = f
	k.wg.Add(1)
	go k.run(f)
	return f
}

// run performs a refresh and publishes its outcome.
func (k *keySet) run(f *flight) {
	defer k.wg.Done()
	ctx, cancel := context.WithTimeout(k.base, httpTimeout)
	defer cancel()
	snap, err := k.fetch(ctx)

	k.mu.Lock()
	if err == nil {
		k.snap.Store(snap)
	}
	k.lastErr = err
	k.inflight = nil
	f.err = err
	k.mu.Unlock()
	close(f.done)

	if err != nil && !errors.Is(err, context.Canceled) {
		k.log.Warn("could not refresh the identity provider's signing keys", slog.Any("error", err))
	}
}

// fetch reads the provider's current keys. A single entry this service cannot read
// is skipped rather than failing the others: the provider may publish key types
// this service does not use next to the ones it does.
func (k *keySet) fetch(ctx context.Context) (*snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch the signing keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch the signing keys: the provider answered %d", resp.StatusCode)
	}

	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode the signing keys: %w", err)
	}
	keys := make(map[string]jose.JSONWebKey, len(set.Keys))
	for _, raw := range set.Keys {
		var key jose.JSONWebKey
		if err := key.UnmarshalJSON(raw); err != nil {
			continue
		}
		// Only public RSA keys meant for signatures, large enough to trust, and
		// that can be told apart.
		pub, isRSA := key.Key.(*rsa.PublicKey)
		if !isRSA || key.KeyID == "" || (key.Use != "" && key.Use != "sig") || pub.N.BitLen() < minRSAKeyBits {
			continue
		}
		keys[key.KeyID] = key
	}
	if len(keys) == 0 {
		return nil, errors.New("the provider published no usable signing keys")
	}
	return &snapshot{keys: keys, fetchedAt: k.now()}, nil
}
