//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/api"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

type walletAPI struct {
	base      string
	client    *contractClient
	strict    *contractClient
	pool      *pgxpool.Pool
	ownerDSN  string
	logs      *logBuffer
	internal  string // tokens
	provider  string // provider-a
	providerB string
}

func startWalletAPI(t *testing.T) walletAPI {
	t.Helper()
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))
	cfg := appConfig(t, db, ls.Endpoint)
	running := startApp(t, cfg)

	internalToken, err := kc.Token(ctx, "internal-service", "internal-service-local-secret")
	require.NoError(t, err)
	providerToken, err := kc.Token(ctx, "provider-a", "provider-a-local-secret")
	require.NoError(t, err)
	providerBToken, err := kc.Token(ctx, "provider-b", "provider-b-local-secret")
	require.NoError(t, err)

	base := "http://" + cfg.HTTPAddr
	strict := newContractClient(t, base)
	return walletAPI{
		base: base, strict: strict, client: strict.lenient(), pool: running.Pool, ownerDSN: db.OwnerDSN, logs: running.Logs,
		internal: internalToken, provider: providerToken, providerB: providerBToken,
	}
}

func (a walletAPI) tables(t *testing.T) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"wallets", "wager_transactions", "wallet_ledger_entries", "outbox"} {
		counts[table] = count(t.Context(), t, a.pool, table)
	}
	return counts
}

func openBody(player uuid.UUID, amount, currency string) string {
	return fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":%q}}`, player, amount, currency)
}

type walletJSON struct {
	ID       string `json:"id"`
	PlayerID string `json:"playerId"`
	Balance  struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"balance"`
	Version int64 `json:"version"`
}

func TestWalletEndpointsFollowTheOpenAPIContract(t *testing.T) {
	a := startWalletAPI(t)
	ctx := t.Context()
	player := uuid.New()

	status, headers, body := a.strict.call(ctx, http.MethodPost, a.base+"/wallets", a.internal, openBody(player, "100.00", "BRL"), "X-Correlation-Id", "corr-contract-1")
	require.Equal(t, http.StatusCreated, status, body)
	var created walletJSON
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	require.Equal(t, player.String(), created.PlayerID)
	require.Equal(t, "100.00", created.Balance.Amount)
	require.Equal(t, int64(1), created.Version)
	require.Equal(t, "/wallets/"+created.ID, headers.Get("Location"))
	require.Equal(t, "corr-contract-1", headers.Get("X-Correlation-Id"))

	status, _, body = a.strict.call(ctx, http.MethodGet, a.base+"/wallets/"+created.ID, a.internal, "")
	require.Equal(t, http.StatusOK, status, body)
	var read walletJSON
	require.NoError(t, json.Unmarshal([]byte(body), &read))
	require.Equal(t, created, read)

	status, _, _ = a.strict.call(ctx, http.MethodGet, a.base+"/wallets/"+uuid.NewString(), a.internal, "")
	require.Equal(t, http.StatusNotFound, status)

	status, _, body = a.strict.call(ctx, http.MethodPost, a.base+"/wallets", a.internal, openBody(player, "5.00", "BRL"))
	require.Equal(t, http.StatusConflict, status, body)
	var conflict struct {
		Error struct{ Code, WalletID string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &conflict))
	require.Equal(t, "WALLET_ALREADY_EXISTS", conflict.Error.Code)
	require.Equal(t, created.ID, conflict.Error.WalletID, "the answer names the wallet that is already there")

	status, _, body = a.strict.call(ctx, http.MethodPost, a.base+"/wallets", a.internal, openBody(uuid.New(), "0.00", "USD"))
	require.Equal(t, http.StatusCreated, status, body)

	for _, path := range []string{"/health/live", "/health/ready", "/openapi.yaml", "/metrics"} {
		status, _, _ = a.strict.call(ctx, http.MethodGet, a.base+path, "", "")
		require.Equal(t, http.StatusOK, status, path)
	}

	a.strict.requireNoViolations()
}

func TestInvalidRequestsAre400WithTheDocumentedBodyAndOpenNothing(t *testing.T) {
	a := startWalletAPI(t)
	ctx := t.Context()
	player := uuid.New()

	for name, body := range map[string]string{
		"negative":      openBody(player, "-5.00", "BRL"),
		"wrong scale":   openBody(player, "5.0", "BRL"),
		"NaN":           openBody(player, "NaN", "BRL"),
		"unknown coin":  openBody(player, "5.00", "JPY"),
		"no body":       ``,
		"not json":      `nope`,
		"bad player":    openBody(uuid.Nil, "5.00", "BRL"),
		"unknown field": strings.TrimSuffix(openBody(player, "5.00", "BRL"), "}") + `,"role":"admin"}`,
		"amount number": fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":5,"currency":"BRL"}}`, player),
	} {
		status, _, response := a.client.call(ctx, http.MethodPost, a.base+"/wallets", a.internal, body)
		require.Equal(t, http.StatusBadRequest, status, "%s: %s", name, response)
		var parsed struct {
			Error struct{ Code string } `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(response), &parsed), name)
		require.Equal(t, "INVALID_REQUEST", parsed.Error.Code, name)
	}
	status, _, _ := a.client.call(ctx, http.MethodGet, a.base+"/wallets/not-a-uuid", a.internal, "")
	require.Equal(t, http.StatusBadRequest, status)

	require.Equal(t, map[string]int{"wallets": 0, "wager_transactions": 0, "wallet_ledger_entries": 0, "outbox": 0}, a.tables(t))
	a.client.requireNoViolations()
}

func TestDeniedRequestsCreateNothing(t *testing.T) {
	a := startWalletAPI(t)
	ctx := t.Context()
	valid := openBody(uuid.New(), "100.00", "BRL")

	for name, token := range map[string]string{
		"no token":           "",
		"garbage token":      "not.a.token",
		"a forged token":     a.internal[:len(a.internal)-4] + "AAAA",
		"a provider's token": a.provider,
	} {
		want := http.StatusUnauthorized
		if token == a.provider {
			want = http.StatusForbidden
		}
		status, _, body := a.client.call(ctx, http.MethodPost, a.base+"/wallets", token, valid)
		require.Equal(t, want, status, "%s: %s", name, body)

		status, _, _ = a.client.call(ctx, http.MethodGet, a.base+"/wallets/"+uuid.NewString(), token, "")
		require.Equal(t, want, status, name)
	}
	require.Equal(t, map[string]int{"wallets": 0, "wager_transactions": 0, "wallet_ledger_entries": 0, "outbox": 0}, a.tables(t),
		"a denied request creates nothing, asserted on the database")
	a.client.requireNoViolations()
}

func TestTheServedDocumentIsTheEmbeddedOne(t *testing.T) {
	a := startWalletAPI(t)
	status, headers, body := a.strict.call(t.Context(), http.MethodGet, a.base+"/openapi.yaml", "", "")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(api.OpenAPI), body)
	require.Contains(t, headers.Get("Content-Type"), "yaml")

	onDisk, err := os.ReadFile(infra.RepoRoot() + "/api/openapi.yaml")
	require.NoError(t, err)
	require.Equal(t, string(onDisk), body, "the binary serves exactly the file in the repository")
}

func TestTheCorrelationIdAndTheWalletIdAreLogged(t *testing.T) {
	a := startWalletAPI(t)
	_, _, body := a.strict.call(t.Context(), http.MethodPost, a.base+"/wallets", a.internal, openBody(uuid.New(), "1.00", "EUR"), "X-Correlation-Id", "corr-log-1")
	var created walletJSON
	require.NoError(t, json.Unmarshal([]byte(body), &created))

	found := false
	for _, line := range strings.Split(a.logs.String(), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil || entry["msg"] != "wallet opened" {
			continue
		}
		found = true
		require.Equal(t, "corr-log-1", entry["correlationId"])
		require.Equal(t, created.ID, entry["walletId"])
	}
	require.True(t, found, "no 'wallet opened' line in the log")

	// A request without one gets a generated id, returned and usable.
	_, headers, _ := a.strict.call(t.Context(), http.MethodGet, a.base+"/wallets/"+created.ID, a.internal, "")
	_, err := uuid.Parse(headers.Get("X-Correlation-Id"))
	require.NoError(t, err)
}

func TestConcurrentOpensOverHTTPProduceOneWallet(t *testing.T) {
	a := startWalletAPI(t)
	player := uuid.New()
	const workers = 20

	var wg sync.WaitGroup
	statuses := make(chan int, workers)
	ids := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, body := a.strict.call(t.Context(), http.MethodPost, a.base+"/wallets", a.internal, openBody(player, "50.00", "BRL"))
			statuses <- status
			var parsed struct {
				ID    string
				Error struct{ WalletID string } `json:"error"`
			}
			_ = json.Unmarshal([]byte(body), &parsed)
			if status == http.StatusCreated {
				ids <- parsed.ID
			} else {
				ids <- parsed.Error.WalletID
			}
		}()
	}
	wg.Wait()
	close(statuses)
	close(ids)

	created, conflicts := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, workers-1, conflicts)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	require.Len(t, seen, 1, "every answer names the same wallet")
	require.Equal(t, map[string]int{"wallets": 1, "wager_transactions": 1, "wallet_ledger_entries": 1, "outbox": 2}, a.tables(t))
	a.strict.requireNoViolations()
}
