//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// A wallet opened through the API, with the player it belongs to.
type apiWallet struct{ id, player string }

func (a walletAPI) openFunded(t *testing.T, amount string) apiWallet {
	t.Helper()
	player := uuid.New()
	status, _, body := a.strict.call(t.Context(), http.MethodPost, a.base+"/wallets", a.internal, openBody(player, amount, "BRL"))
	require.Equal(t, http.StatusCreated, status, body)
	var created walletJSON
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	return apiWallet{id: created.ID, player: player.String()}
}

func operation(w apiWallet, externalID string, kind, amount string) string {
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,"walletId":%q,
		"roundId":"round-1","gameId":"game-1","kind":%q,"money":{"amount":%q,"currency":"BRL"}}`, externalID, w.player, w.id, kind, amount)
}

type operationAnswer struct {
	TransactionID    string `json:"transactionId"`
	Status           string `json:"status"`
	FailureCode      string `json:"failureCode"`
	IdempotentReplay bool   `json:"idempotentReplay"`
	Balance          struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"balance"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func (a walletAPI) submit(t *testing.T, client *contractClient, token, key, body string, headers ...string) (int, http.Header, operationAnswer) {
	t.Helper()
	all := headers
	if key != "" {
		all = append([]string{"Idempotency-Key", key}, headers...)
	}
	status, header, raw := client.call(t.Context(), http.MethodPost, a.base+"/wagering/transactions", token, body, all...)
	var answer operationAnswer
	_ = json.Unmarshal([]byte(raw), &answer)
	return status, header, answer
}

func TestOperationsFollowTheOpenAPIContractInEveryShape(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "1000.00")

	status, headers, processed := a.submit(t, a.strict, a.provider, "provider-a:tx-1", operation(w, "tx-1", "BET", "25.00"), "X-Correlation-Id", "corr-api-1")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "PROCESSED", processed.Status)
	require.Equal(t, "975.00", processed.Balance.Amount)
	require.False(t, processed.IdempotentReplay)
	require.Equal(t, "corr-api-1", headers.Get("X-Correlation-Id"))

	// More movements on the wallet, then the same request again.
	status, _, _ = a.submit(t, a.strict, a.provider, "provider-a:tx-2", operation(w, "tx-2", "BET", "100.00"))
	require.Equal(t, http.StatusOK, status)

	status, _, replay := a.submit(t, a.strict, a.provider, "provider-a:tx-1", operation(w, "tx-1", "BET", "25.00"))
	require.Equal(t, http.StatusOK, status)
	require.True(t, replay.IdempotentReplay)
	require.Equal(t, processed.TransactionID, replay.TransactionID)
	require.Equal(t, "975.00", replay.Balance.Amount, "the balance of the first time, not today's 875.00")

	status, _, rejected := a.submit(t, a.strict, a.provider, "provider-a:tx-big", operation(w, "tx-big", "BET", "5000.00"))
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "REJECTED", rejected.Status)
	require.Equal(t, "INSUFFICIENT_FUNDS", rejected.FailureCode)
	status, _, again := a.submit(t, a.strict, a.provider, "provider-a:tx-big", operation(w, "tx-big", "BET", "5000.00"))
	require.Equal(t, http.StatusUnprocessableEntity, status, "a rejection is final: the same request is rejected again")
	require.True(t, again.IdempotentReplay)

	missing := apiWallet{id: uuid.NewString(), player: w.player}
	status, _, notFound := a.submit(t, a.strict, a.provider, "provider-a:tx-ghost", operation(missing, "tx-ghost", "BET", "1.00"))
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "WALLET_NOT_FOUND", notFound.FailureCode)

	status, _, conflict := a.submit(t, a.strict, a.provider, "provider-a:tx-1", operation(w, "tx-1", "BET", "26.00"))
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "IDEMPOTENCY_KEY_CONFLICT", conflict.Error.Code)
	status, _, duplicate := a.submit(t, a.strict, a.provider, "another-key", operation(w, "tx-1", "BET", "25.00"))
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "DUPLICATE_EXTERNAL_TRANSACTION", duplicate.Error.Code)

	a.strict.requireNoViolations()

	var balance, entries int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT balance_minor FROM wallets WHERE id = $1`, w.id).Scan(&balance))
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id).Scan(&entries))
	require.Equal(t, 87500, balance)
	require.Equal(t, 2, entries, "two bets applied, no more")
}

func TestInvalidOperationsAre400WithTheDocumentedBodyAndStoreNothing(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	before := a.tables(t)

	valid := operation(w, "tx-1", "BET", "25.00")
	replaceInBody := func(old, new string) string { return strings.Replace(valid, old, new, 1) }
	cases := map[string]struct {
		key  string
		body string
	}{
		"no idempotency key":       {"", valid},
		"a negative amount":        {"k", replaceInBody(`"25.00"`, `"-25.00"`)},
		"an amount with 1 decimal": {"k", replaceInBody(`"25.00"`, `"25.0"`)},
		"NaN":                      {"k", replaceInBody(`"25.00"`, `"NaN"`)},
		"an opening":               {"k", replaceInBody(`"BET"`, `"OPENING"`)},
		"an unknown kind":          {"k", replaceInBody(`"BET"`, `"TRANSFER"`)},
		"a kind not processed yet": {"k", replaceInBody(`"BET"`, `"WIN"`)},
		"a zero bet":               {"k", replaceInBody(`"25.00"`, `"0.00"`)},
		"a bad player":             {"k", replaceInBody(w.player, "nope")},
		"no body":                  {"k", ""},
		"an unknown field":         {"k", strings.TrimSuffix(valid, "}") + `,"extra":1}`},
	}
	for name, c := range cases {
		status, _, answer := a.submit(t, a.client, a.provider, c.key, c.body)
		require.Equal(t, http.StatusBadRequest, status, name)
		require.Equal(t, "INVALID_REQUEST", answer.Error.Code, name)
	}
	require.Equal(t, before, a.tables(t), "nothing was stored")
	a.client.requireNoViolations()
}

func TestADeniedOperationCreatesNothing(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	before := a.tables(t)
	body := operation(w, "tx-1", "BET", "25.00") // for provider-a

	for name, c := range map[string]struct {
		token string
		want  int
	}{
		"another provider's identity": {a.providerB, http.StatusForbidden},
		"the internal role":           {a.internal, http.StatusForbidden},
		"no token":                    {"", http.StatusUnauthorized},
		"a forged token":              {a.provider[:len(a.provider)-4] + "AAAA", http.StatusUnauthorized},
	} {
		status, _, _ := a.submit(t, a.client, c.token, "provider-a:tx-1", body)
		require.Equal(t, c.want, status, name)
	}
	require.Equal(t, before, a.tables(t), "a denied request creates no rows")

	// Acting for another provider, naming it in the body, is no way around it.
	forged := strings.Replace(body, `"provider-a"`, `"provider-b"`, 1)
	status, _, _ := a.submit(t, a.client, a.provider, "provider-b:tx-1", forged)
	require.Equal(t, http.StatusForbidden, status)
	require.Equal(t, before, a.tables(t))
	a.client.requireNoViolations()
}

func TestAProviderNeverSeesAnotherProvidersKeysOrIds(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "1000.00")
	status, _, _ := a.submit(t, a.strict, a.provider, "shared-key", operation(w, "shared-id", "BET", "25.00"))
	require.Equal(t, http.StatusOK, status)

	// Provider B uses the very same key and external id: its own namespace, not a replay
	// or a conflict with provider A's operation.
	bodyB := strings.Replace(operation(w, "shared-id", "BET", "25.00"), `"provider-a"`, `"provider-b"`, 1)
	status, _, answer := a.submit(t, a.strict, a.providerB, "shared-key", bodyB)
	require.Equal(t, http.StatusOK, status)
	require.False(t, answer.IdempotentReplay)
	a.strict.requireNoViolations()
}

func TestFiftySequentialReplaysHaveOneEffect(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	body := operation(w, "tx-1", "BET", "30.00")

	var first operationAnswer
	for i := 0; i < 50; i++ {
		status, _, answer := a.submit(t, a.strict, a.provider, "provider-a:tx-1", body)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "70.00", answer.Balance.Amount)
		if i == 0 {
			first = answer
			require.False(t, answer.IdempotentReplay)
			continue
		}
		require.True(t, answer.IdempotentReplay, "request %d", i)
		require.Equal(t, first.TransactionID, answer.TransactionID)
	}

	var balance, entries, events int
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT balance_minor FROM wallets WHERE id = $1`, w.id).Scan(&balance))
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`, w.id).Scan(&entries))
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE payload->>'transactionId' = $1`, first.TransactionID).Scan(&events))
	require.Equal(t, 7000, balance)
	require.Equal(t, 1, entries)
	require.Equal(t, 2, events, "the events were written once")
	a.strict.requireNoViolations()
}

func TestTheCorrelationIdIsStoredAndNeitherTokensNorPayloadsAreLogged(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")

	status, headers, answer := a.submit(t, a.strict, a.provider, "provider-a:tx-log", operation(w, "tx-log", "BET", "12.34"), "X-Correlation-Id", "corr-op-9")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "corr-op-9", headers.Get("X-Correlation-Id"))

	var correlation string
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT correlation_id FROM wager_transactions WHERE id = $1`, answer.TransactionID).Scan(&correlation))
	require.Equal(t, "corr-op-9", correlation)
	var eventCorrelation string
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT correlation_id FROM outbox WHERE payload->>'transactionId' = $1 LIMIT 1`, answer.TransactionID).Scan(&eventCorrelation))
	require.Equal(t, "corr-op-9", eventCorrelation, "and it follows the events")

	logged := a.logs.String()
	require.Contains(t, logged, "corr-op-9")
	require.Contains(t, logged, answer.TransactionID)
	for _, secret := range []string{a.provider, a.providerB, a.internal, "12.34", "fortune", "game-1", "provider-a:tx-log"} {
		require.NotContains(t, logged, secret, "the log must carry no token, amount, game or idempotency key")
	}
}

// With the database stopped, the answer is 503 with Retry-After, and the request, being
// idempotent, can be sent again once the database is back.
func TestWhileTheDatabaseIsStoppedTheAnswerIs503WithRetryAfter(t *testing.T) {
	ctx := t.Context()
	own, err := infra.StartPostgres(ctx)
	if own != nil && own.Container != nil {
		t.Cleanup(func() { _ = own.Container.Terminate(context.Background()) })
	}
	require.NoError(t, err)
	db := own.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))

	cfg := appConfig(t, db, ls.Endpoint)
	running := startApp(t, cfg)
	_ = running
	base := "http://" + cfg.HTTPAddr
	client := newContractClient(t, base)
	internalToken, err := kc.Token(ctx, "internal-service", "internal-service-local-secret")
	require.NoError(t, err)
	providerToken, err := kc.Token(ctx, "provider-a", "provider-a-local-secret")
	require.NoError(t, err)

	player := uuid.New()
	status, _, body := client.call(ctx, http.MethodPost, base+"/wallets", internalToken, openBody(player, "100.00", "BRL"))
	require.Equal(t, http.StatusCreated, status, body)
	var created walletJSON
	require.NoError(t, json.Unmarshal([]byte(body), &created))
	w := apiWallet{id: created.ID, player: player.String()}

	a := walletAPI{base: base}
	status, _, _ = a.submit(t, client, providerToken, "k-before", operation(w, "tx-before", "BET", "10.00"))
	require.Equal(t, http.StatusOK, status, "it works while the database is up")

	stopTimeout := 10 * time.Second
	require.NoError(t, own.Container.Stop(ctx, &stopTimeout))

	status, headers, answer := a.submit(t, client, providerToken, "k-during", operation(w, "tx-during", "BET", "10.00"))
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Equal(t, "UNAVAILABLE", answer.Error.Code)
	require.NotEmpty(t, headers.Get("Retry-After"))

	status, headers, _ = client.call(ctx, http.MethodGet, base+"/wallets/"+w.id, internalToken, "")
	require.Equal(t, http.StatusServiceUnavailable, status, "reads are told the same")
	require.NotEmpty(t, headers.Get("Retry-After"))
	client.requireNoViolations()
}
