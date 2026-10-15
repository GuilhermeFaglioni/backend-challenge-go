package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

var (
	wagerPlayer = uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7")
	wagerWallet = uuid.MustParse("3fa85f64-5717-4562-b3fc-2c963f66afa6")
)

type fakeProcessor struct {
	calls  []processwager.Input
	output processwager.Output
	err    error
}

func (f *fakeProcessor) Execute(_ context.Context, in processwager.Input) (processwager.Output, error) {
	f.calls = append(f.calls, in)
	return f.output, f.err
}

type wageringEnv struct {
	processor *fakeProcessor
	routes    *Routes
	registry  *prometheus.Registry
	logs      *bytes.Buffer
}

func processedBet(t *testing.T, replay bool) processwager.Output {
	t.Helper()
	amount, err := money.FromMinor(2500, money.BRL)
	require.NoError(t, err)
	request, err := wager.NewRequest(wager.RequestParams{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123", PlayerID: wagerPlayer, WalletID: wagerWallet,
		RoundID: "round-987", GameID: "fortune-chimp", Kind: wager.KindBet, Amount: amount,
	})
	require.NoError(t, err)
	tx, err := wager.New(wager.NewParams{ID: uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d"), Request: request, IdempotencyKey: "k", CorrelationID: "c", At: now})
	require.NoError(t, err)
	balance, err := money.FromMinor(97500, money.BRL)
	require.NoError(t, err)
	done, err := tx.Process(wager.Result{Balance: balance, WalletVersion: 4}, now.Add(time.Second))
	require.NoError(t, err)
	return processwager.Output{Transaction: done, IdempotentReplay: replay}
}

func rejectedBet(t *testing.T, code wager.FailureCode, replay bool) processwager.Output {
	t.Helper()
	out := processedBet(t, false)
	amount, err := money.FromMinor(2500, money.BRL)
	require.NoError(t, err)
	request, err := wager.NewRequest(wager.RequestParams{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123", PlayerID: wagerPlayer, WalletID: wagerWallet,
		RoundID: "round-987", GameID: "fortune-chimp", Kind: wager.KindBet, Amount: amount,
	})
	require.NoError(t, err)
	tx, err := wager.New(wager.NewParams{ID: out.Transaction.ID(), Request: request, IdempotencyKey: "k", CorrelationID: "c", At: now})
	require.NoError(t, err)
	rejected, err := tx.Reject(code, now.Add(time.Second))
	require.NoError(t, err)
	return processwager.Output{Transaction: rejected, IdempotentReplay: replay}
}

func newWageringEnv(t *testing.T) *wageringEnv {
	t.Helper()
	e := &wageringEnv{processor: &fakeProcessor{output: processedBet(t, false)}, registry: prometheus.NewRegistry(), logs: &bytes.Buffer{}}
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	e.routes = NewRoutes(fakeAuthenticator{
		"provider-a-token": {principal: providerA},
		"provider-b-token": {principal: providerB},
		"internal-token":   {principal: internal},
	}, logger, e.registry)
	NewWagering(e.processor, logger).register(e.routes)
	return e
}

func validOperation() string {
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":%q,"walletId":%q,
		"roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`, wagerPlayer, wagerWallet)
}

// withField returns the valid operation with one field replaced by the given JSON (or
// removed, when the JSON is empty).
func withField(field, replacement string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(validOperation()), &fields); err != nil {
		panic(err)
	}
	if replacement == "" {
		delete(fields, field)
	} else {
		fields[field] = json.RawMessage(replacement)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func (e *wageringEnv) post(token, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/wagering/transactions", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.routes.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *wageringEnv) send(body string) *httptest.ResponseRecorder {
	return e.post("provider-a-token", body, "Idempotency-Key", "provider-a:transaction-123")
}

// --- the answers ---------------------------------------------------------------------

func TestAProcessedOperationAnswers200WithTheBalance(t *testing.T) {
	e := newWageringEnv(t)
	rec := e.send(validOperation())

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"transactionId":"0192f298-345e-7e38-af88-e43f851a819d","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":false}`, rec.Body.String())
}

func TestAReplayAnswers200AndSaysSo(t *testing.T) {
	e := newWageringEnv(t)
	e.processor.output = processedBet(t, true)
	rec := e.send(validOperation())
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"transactionId":"0192f298-345e-7e38-af88-e43f851a819d","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":true}`, rec.Body.String())
}

func TestARejectionAnswers422WithItsCode(t *testing.T) {
	for _, code := range wager.FailureCodes() {
		if !code.IsRejection() {
			continue
		}
		e := newWageringEnv(t)
		e.processor.output = rejectedBet(t, code, false)
		rec := e.send(validOperation())
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, code)
		require.JSONEq(t, fmt.Sprintf(`{"transactionId":"0192f298-345e-7e38-af88-e43f851a819d","status":"REJECTED","failureCode":%q,"idempotentReplay":false}`, code), rec.Body.String())
	}
	e := newWageringEnv(t)
	e.processor.output = rejectedBet(t, wager.FailureInsufficientFunds, true)
	rec := e.send(validOperation())
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "a replay of a rejection is the same rejection")
	require.Contains(t, rec.Body.String(), `"idempotentReplay":true`)
}

func TestTheTwoConflictsAre409WithTheirOwnCodes(t *testing.T) {
	for err, code := range map[error]string{
		processwager.ErrIdempotencyKeyConflict:       CodeIdempotencyKeyConflict,
		processwager.ErrDuplicateExternalTransaction: CodeDuplicateExternalTransaction,
	} {
		e := newWageringEnv(t)
		e.processor.err = fmt.Errorf("wrapped: %w", err)
		rec := e.send(validOperation())
		require.Equal(t, http.StatusConflict, rec.Code)
		require.Equal(t, code, decodeError(t, rec).Error.Code)
	}
}

func TestWageringFailuresAreMappedWithoutLeakingInternals(t *testing.T) {
	secret := "password=hunter2 host=10.0.0.5"
	cases := map[string]struct {
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		"temporarily unavailable":  {fmt.Errorf("%w: %s", application.ErrTemporarilyUnavailable, secret), http.StatusServiceUnavailable, CodeUnavailable, true},
		"a kind not supported yet": {fmt.Errorf("%w: WIN", processwager.ErrUnsupportedKind), http.StatusBadRequest, CodeInvalidRequest, false},
		"invalid input":            {fmt.Errorf("%w: wager: invalid identifier: roundId", processwager.ErrInvalidInput), http.StatusBadRequest, CodeInvalidRequest, false},
		"anything else":            {errors.New(secret), http.StatusInternalServerError, CodeInternal, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newWageringEnv(t)
			e.processor.err = c.err
			rec := e.send(validOperation())
			require.Equal(t, c.status, rec.Code)
			require.Equal(t, c.code, decodeError(t, rec).Error.Code)
			require.Equal(t, c.retryAfter, rec.Header().Get("Retry-After") != "")
			require.NotContains(t, rec.Body.String(), "hunter2", "internal detail reached the client")
			require.NotContains(t, rec.Body.String(), "10.0.0.5")
		})
	}
}

// --- who may call, and for whom ------------------------------------------------------

func TestAProviderCanOnlyActForItself(t *testing.T) {
	e := newWageringEnv(t)
	rec := e.post("provider-b-token", validOperation(), "Idempotency-Key", "k") // the body says provider-a
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, CodeForbidden, decodeError(t, rec).Error.Code)
	require.Empty(t, e.processor.calls, "authorization comes before any effect")
	require.InDelta(t, 1, failureCount(t, e.registry, auth.ReasonProviderMismatch), 0, "and it is counted")
}

func TestTheProviderIsDecidedBeforeTheRestOfTheRequestIsLookedAt(t *testing.T) {
	// A request that is both another provider's and badly formed is refused as another
	// provider's: nothing about it is read further.
	e := newWageringEnv(t)
	rec := e.post("provider-b-token", withField("money", `"not money"`), "Idempotency-Key", "k")
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = e.post("provider-b-token", validOperation()) // and without the key, too
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Empty(t, e.processor.calls)
}

func TestWhoseOperationItIsComesFromTheTokenNotTheBody(t *testing.T) {
	e := newWageringEnv(t)
	rec := e.send(validOperation())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, e.processor.calls, 1)
	require.Equal(t, "provider-a", e.processor.calls[0].Request.ProviderID)
}

func TestOnlyTheProviderRoleMayCallIt(t *testing.T) {
	e := newWageringEnv(t)
	rec := e.post("internal-token", validOperation(), "Idempotency-Key", "k")
	require.Equal(t, http.StatusForbidden, rec.Code, "the internal service does not submit operations")
	rec = e.post("", validOperation(), "Idempotency-Key", "k")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = e.post("garbage", validOperation(), "Idempotency-Key", "k")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Empty(t, e.processor.calls)
}

func TestTheRouteIsProtectedByTheProviderRoleOnly(t *testing.T) {
	e := newWageringEnv(t)
	routes := e.routes.Registered()
	require.Len(t, routes, 1)
	require.Equal(t, Route{Pattern: "POST /wagering/transactions", Roles: []auth.Role{auth.RoleProvider}}, routes[0])
}

// --- the Idempotency-Key header ------------------------------------------------------

func TestTheIdempotencyKeyIsRequiredAndKeptAsReceived(t *testing.T) {
	e := newWageringEnv(t)
	rec := e.post("provider-a-token", validOperation(), "Idempotency-Key", "my-own-key-123")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "my-own-key-123", e.processor.calls[0].IdempotencyKey, "not replaced by one built from the body")

	for name, headers := range map[string][]string{
		"absent":       nil,
		"empty":        {"Idempotency-Key", ""},
		"with a space": {"Idempotency-Key", "two words"},
		"not ASCII":    {"Idempotency-Key", "chave-ção"},
		"too long":     {"Idempotency-Key", strings.Repeat("k", 256)},
		"given twice":  {"Idempotency-Key", "one", "Idempotency-Key", "two"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newWageringEnv(t)
			rec := e.post("provider-a-token", validOperation(), headers...)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			body := decodeError(t, rec)
			require.Equal(t, CodeInvalidRequest, body.Error.Code)
			require.Equal(t, "Idempotency-Key", body.Error.Details[0].Field)
			require.Empty(t, e.processor.calls)
		})
	}

	long := strings.Repeat("k", 255)
	rec = e.post("provider-a-token", validOperation(), "Idempotency-Key", long)
	require.Equal(t, http.StatusOK, rec.Code, "255 characters is the limit")
}

// --- the body ------------------------------------------------------------------------

func TestInvalidBodiesAre400AndReachNoUseCase(t *testing.T) {
	cases := map[string]struct {
		body  string
		field string
	}{
		"externalTransactionId missing":  {withField("externalTransactionId", ""), "externalTransactionId"},
		"externalTransactionId a number": {withField("externalTransactionId", `12`), "externalTransactionId"},
		"externalTransactionId empty":    {withField("externalTransactionId", `""`), "externalTransactionId"},
		"externalTransactionId padded":   {withField("externalTransactionId", `" x"`), "externalTransactionId"},
		"externalTransactionId too long": {withField("externalTransactionId", `"`+strings.Repeat("a", 129)+`"`), "externalTransactionId"},
		"playerId missing":               {withField("playerId", ""), "playerId"},
		"playerId not a uuid":            {withField("playerId", `"nope"`), "playerId"},
		"playerId nil":                   {withField("playerId", `"00000000-0000-0000-0000-000000000000"`), "playerId"},
		"walletId missing":               {withField("walletId", ""), "walletId"},
		"walletId without dashes":        {withField("walletId", `"3fa85f6457174562b3fc2c963f66afa6"`), "walletId"},
		"roundId missing":                {withField("roundId", ""), "roundId"},
		"roundId empty":                  {withField("roundId", `""`), "roundId"},
		"gameId missing":                 {withField("gameId", ""), "gameId"},
		"kind missing":                   {withField("kind", ""), "kind"},
		"kind lower case":                {withField("kind", `"bet"`), "kind"},
		"kind unknown":                   {withField("kind", `"TRANSFER"`), "kind"},
		"kind OPENING":                   {withField("kind", `"OPENING"`), "kind"},
		"money missing":                  {withField("money", ""), "money"},
		"money null":                     {withField("money", `null`), "money"},
		"money a string":                 {withField("money", `"25.00"`), "money"},
		"amount negative":                {withField("money", `{"amount":"-1.00","currency":"BRL"}`), "money"},
		"amount one decimal":             {withField("money", `{"amount":"25.0","currency":"BRL"}`), "money"},
		"amount a number":                {withField("money", `{"amount":25.00,"currency":"BRL"}`), "money"},
		"amount NaN":                     {withField("money", `{"amount":"NaN","currency":"BRL"}`), "money"},
		"amount zero for a bet":          {withField("money", `{"amount":"0.00","currency":"BRL"}`), "money"},
		"currency unknown":               {withField("money", `{"amount":"25.00","currency":"JPY"}`), "money"},
		"a bet with a reference":         {withField("referenceExternalTransactionId", `"transaction-100"`), "referenceExternalTransactionId"},
		"reference a number":             {withField("referenceExternalTransactionId", `5`), "referenceExternalTransactionId"},
		"providerId a number":            {withField("providerId", `1`), "providerId"},
		"providerId missing":             {withField("providerId", ""), "providerId"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newWageringEnv(t)
			rec := e.send(c.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			body := decodeError(t, rec)
			require.Equal(t, CodeInvalidRequest, body.Error.Code)
			require.NotEmpty(t, body.Error.Details)
			require.Equal(t, c.field, body.Error.Details[0].Field)
			require.NotEmpty(t, body.Error.Details[0].Issue)
			require.Empty(t, e.processor.calls)
		})
	}
}

func TestMalformedBodiesAre400(t *testing.T) {
	for name, body := range map[string]string{
		"empty":            "",
		"not json":         "hello",
		"an array":         "[]",
		"truncated":        `{"providerId":`,
		"an unknown field": strings.TrimSuffix(validOperation(), "}") + `,"idempotencyKey":"smuggled"}`,
		"two documents":    validOperation() + validOperation(),
		"trailing garbage": validOperation() + "x",
		"too large":        validOperation() + strings.Repeat(" ", maxBodyBytes),
	} {
		t.Run(name, func(t *testing.T) {
			e := newWageringEnv(t)
			rec := e.send(body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, CodeInvalidRequest, decodeError(t, rec).Error.Code)
			require.Empty(t, e.processor.calls)
		})
	}
}

func TestTheUseCaseGetsExactlyWhatWasSent(t *testing.T) {
	e := newWageringEnv(t)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(validOperation()), &fields))
	fields["kind"] = json.RawMessage(`"REFUND"`)
	fields["referenceExternalTransactionId"] = json.RawMessage(`"transaction-100"`)
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	body := string(encoded)
	rec := e.post("provider-a-token", body, "Idempotency-Key", "provider-a:transaction-123", "X-Correlation-Id", "corr-77")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := e.processor.calls[0]
	require.Equal(t, wager.RequestParams{
		ProviderID: "provider-a", ExternalTransactionID: "transaction-123", PlayerID: wagerPlayer, WalletID: wagerWallet,
		RoundID: "round-987", GameID: "fortune-chimp", Kind: wager.KindRefund, Amount: got.Request.Amount,
		ReferenceExternalTransactionID: "transaction-100",
	}, got.Request)
	require.Equal(t, int64(2500), got.Request.Amount.Minor())
	require.Equal(t, "provider-a:transaction-123", got.IdempotencyKey)
	require.Equal(t, "corr-77", got.CorrelationID)
	require.Equal(t, "corr-77", rec.Header().Get("X-Correlation-Id"))
}

// --- observability --------------------------------------------------------------------

func TestNeitherTheTokenNorThePayloadIsLogged(t *testing.T) {
	e := newWageringEnv(t)
	e.processor.err = errors.New("boom")
	_ = e.send(validOperation())
	_ = e.post("provider-b-token", validOperation(), "Idempotency-Key", "k")
	for _, secret := range []string{"provider-a-token", "provider-b-token", "25.00", "fortune-chimp", "round-987", "provider-a:transaction-123"} {
		require.NotContains(t, e.logs.String(), secret)
	}
}

func failureCount(t *testing.T, reg *prometheus.Registry, reason auth.Reason) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "auth_failures_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			if labelValue(m, "reason") == string(reason) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
