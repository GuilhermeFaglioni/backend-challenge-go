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
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

var now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

type fakeOpener struct {
	calls  []openwallet.Input
	result wallet.Wallet
	err    error
}

func (f *fakeOpener) Execute(_ context.Context, in openwallet.Input) (wallet.Wallet, error) {
	f.calls = append(f.calls, in)
	return f.result, f.err
}

type fakeReader struct {
	calls  []uuid.UUID
	result wallet.Wallet
	err    error
}

func (f *fakeReader) ByID(_ context.Context, id uuid.UUID) (wallet.Wallet, error) {
	f.calls = append(f.calls, id)
	return f.result, f.err
}

func fundedWallet(t *testing.T, minor int64) wallet.Wallet {
	t.Helper()
	initial, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	w, _, err := wallet.OpenWithBalance(uuid.New(), uuid.New(), initial, uuid.New(), now)
	require.NoError(t, err)
	return w
}

type walletEnv struct {
	opener *fakeOpener
	reader *fakeReader
	routes *Routes
	logs   *bytes.Buffer
}

func newWalletEnv(t *testing.T) *walletEnv {
	t.Helper()
	e := &walletEnv{opener: &fakeOpener{result: fundedWallet(t, 10000)}, reader: &fakeReader{}, logs: &bytes.Buffer{}}
	e.reader.result = e.opener.result
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	e.routes = NewRoutes(fakeAuthenticator{
		"internal-token": {principal: internal},
		"provider-token": {principal: providerA},
	}, logger, prometheus.NewRegistry())
	NewWallets(e.opener, e.reader, logger).register(e.routes)
	return e
}

func (e *walletEnv) do(method, path, token, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
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

func validBody(player uuid.UUID) string {
	return fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, player)
}

type errorResponse struct {
	Error struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		WalletID string `json:"walletId"`
		Details  []struct {
			Field string `json:"field"`
			Issue string `json:"issue"`
		} `json:"details"`
	} `json:"error"`
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var body errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body
}

// --- POST /wallets -----------------------------------------------------------------

func TestOpeningAWalletAnswers201WithTheWalletAndWhereToReadIt(t *testing.T) {
	e := newWalletEnv(t)
	player := uuid.New()

	rec := e.do(http.MethodPost, "/wallets", "internal-token", validBody(player), "X-Correlation-Id", "corr-123")

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	w := e.opener.result
	require.Equal(t, "/wallets/"+w.ID().String(), rec.Header().Get("Location"))
	require.JSONEq(t, fmt.Sprintf(`{"id":%q,"playerId":%q,"balance":{"amount":"100.00","currency":"BRL"},"version":1}`, w.ID(), w.PlayerID()), rec.Body.String())
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	require.Len(t, e.opener.calls, 1)
	call := e.opener.calls[0]
	require.Equal(t, player, call.PlayerID)
	require.Equal(t, int64(10000), call.InitialBalance.Minor())
	require.Equal(t, money.BRL, call.InitialBalance.Currency())
	require.Equal(t, "corr-123", call.CorrelationID)
	require.Equal(t, "corr-123", rec.Header().Get("X-Correlation-Id"))
}

func TestAWalletOpenedWithZeroIsAccepted(t *testing.T) {
	e := newWalletEnv(t)
	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"0.00","currency":"USD"}}`, uuid.New())
	rec := e.do(http.MethodPost, "/wallets", "internal-token", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.True(t, e.opener.calls[0].InitialBalance.IsZero())
}

func TestInvalidBodiesAre400AndOpenNothing(t *testing.T) {
	player := uuid.New().String()
	money := func(amount, currency string) string {
		return fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":%q}}`, player, amount, currency)
	}
	cases := map[string]struct {
		body  string
		field string
	}{
		"negative amount":       {money("-5.00", "BRL"), "initialBalance"},
		"no decimals":           {money("5", "BRL"), "initialBalance"},
		"one decimal":           {money("5.0", "BRL"), "initialBalance"},
		"three decimals":        {money("5.000", "BRL"), "initialBalance"},
		"NaN":                   {money("NaN", "BRL"), "initialBalance"},
		"exponent":              {money("1e3", "BRL"), "initialBalance"},
		"leading zero":          {money("05.00", "BRL"), "initialBalance"},
		"comma":                 {money("5,00", "BRL"), "initialBalance"},
		"unknown currency":      {money("5.00", "JPY"), "initialBalance"},
		"lower-case currency":   {money("5.00", "brl"), "initialBalance"},
		"amount as a number":    {fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":5.00,"currency":"BRL"}}`, player), "initialBalance"},
		"balance as a string":   {fmt.Sprintf(`{"playerId":%q,"initialBalance":"5.00"}`, player), "initialBalance"},
		"balance missing":       {fmt.Sprintf(`{"playerId":%q}`, player), "initialBalance"},
		"balance null":          {fmt.Sprintf(`{"playerId":%q,"initialBalance":null}`, player), "initialBalance"},
		"player missing":        {`{"initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"player null":           {`{"playerId":null,"initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"player not a uuid":     {`{"playerId":"not-a-uuid","initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"player nil uuid":       {`{"playerId":"00000000-0000-0000-0000-000000000000","initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"player without dashes": {`{"playerId":"7c9e6679742540de944be07fc1f90ae7","initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"player in braces":      {`{"playerId":"{7c9e6679-7425-40de-944b-e07fc1f90ae7}","initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"player a number":       {`{"playerId":42,"initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newWalletEnv(t)
			rec := e.do(http.MethodPost, "/wallets", "internal-token", c.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			body := decodeError(t, rec)
			require.Equal(t, CodeInvalidRequest, body.Error.Code)
			require.NotEmpty(t, body.Error.Details)
			require.Equal(t, c.field, body.Error.Details[0].Field)
			require.NotEmpty(t, body.Error.Details[0].Issue)
			require.Empty(t, e.opener.calls, "nothing reaches the use case")
		})
	}
}

func TestMalformedRequestsAre400(t *testing.T) {
	// Valid JSON that is only too big: the cap must be what refuses it.
	big := validBody(uuid.New()) + strings.Repeat(" ", maxBodyBytes)
	for name, body := range map[string]string{
		"empty body":       "",
		"not json":         "hello",
		"truncated json":   `{"playerId":`,
		"an array":         `[]`,
		"a string":         `"x"`,
		"unknown field":    validBody(uuid.New())[:len(validBody(uuid.New()))-1] + `,"extra":1}`,
		"two documents":    validBody(uuid.New()) + validBody(uuid.New()),
		"trailing garbage": validBody(uuid.New()) + "x",
		"too large":        big,
	} {
		t.Run(name, func(t *testing.T) {
			e := newWalletEnv(t)
			rec := e.do(http.MethodPost, "/wallets", "internal-token", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, CodeInvalidRequest, decodeError(t, rec).Error.Code)
			require.Empty(t, e.opener.calls)
		})
	}
}

func TestAMissingOrNullFieldIsReportedAsRequired(t *testing.T) {
	player := uuid.New().String()
	for name, c := range map[string]struct{ body, field string }{
		"player missing":  {`{"initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"player null":     {`{"playerId":null,"initialBalance":{"amount":"5.00","currency":"BRL"}}`, "playerId"},
		"balance missing": {fmt.Sprintf(`{"playerId":%q}`, player), "initialBalance"},
		"balance null":    {fmt.Sprintf(`{"playerId":%q,"initialBalance":null}`, player), "initialBalance"},
	} {
		e := newWalletEnv(t)
		rec := e.do(http.MethodPost, "/wallets", "internal-token", c.body)
		body := decodeError(t, rec)
		require.Len(t, body.Error.Details, 1, name)
		require.Equal(t, c.field, body.Error.Details[0].Field, name)
		require.Equal(t, "is required", body.Error.Details[0].Issue, name)
	}
}

func TestOneBadFieldDoesNotHideTheOther(t *testing.T) {
	e := newWalletEnv(t)
	rec := e.do(http.MethodPost, "/wallets", "internal-token", `{"playerId":"nope","initialBalance":{"amount":"5","currency":"BRL"}}`)
	body := decodeError(t, rec)
	var fields []string
	for _, d := range body.Error.Details {
		fields = append(fields, d.Field)
	}
	require.ElementsMatch(t, []string{"playerId", "initialBalance"}, fields)
}

func TestAnExistingWalletIs409WithItsId(t *testing.T) {
	e := newWalletEnv(t)
	existing := uuid.New()
	e.opener.err = fmt.Errorf("wrapped: %w", &application.WalletAlreadyExistsError{WalletID: existing})

	rec := e.do(http.MethodPost, "/wallets", "internal-token", validBody(uuid.New()))
	require.Equal(t, http.StatusConflict, rec.Code)
	body := decodeError(t, rec)
	require.Equal(t, CodeWalletAlreadyExists, body.Error.Code)
	require.Equal(t, existing.String(), body.Error.WalletID)
}

func TestFailuresAreMappedWithoutLeakingInternals(t *testing.T) {
	secret := "password=hunter2 host=10.0.0.5"
	cases := map[string]struct {
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		"temporarily unavailable": {fmt.Errorf("%w: %s", application.ErrTemporarilyUnavailable, secret), http.StatusServiceUnavailable, CodeUnavailable, true},
		"invalid input":           {fmt.Errorf("%w: wallet: identifier must not be empty", openwallet.ErrInvalidInput), http.StatusBadRequest, CodeInvalidRequest, false},
		"anything else":           {errors.New(secret), http.StatusInternalServerError, CodeInternal, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newWalletEnv(t)
			e.opener.err = c.err
			rec := e.do(http.MethodPost, "/wallets", "internal-token", validBody(uuid.New()), "X-Correlation-Id", "corr-9")
			require.Equal(t, c.status, rec.Code)
			require.Equal(t, c.code, decodeError(t, rec).Error.Code)
			require.Equal(t, c.retryAfter, rec.Header().Get("Retry-After") != "")
			if c.status != http.StatusBadRequest {
				require.NotContains(t, rec.Body.String(), "hunter2", "internal detail reached the client")
				require.NotContains(t, rec.Body.String(), "10.0.0.5")
			}
			if c.status == http.StatusInternalServerError {
				require.Contains(t, e.logs.String(), "hunter2", "but the log has it, with the correlation id")
				require.Contains(t, e.logs.String(), "corr-9")
			}
		})
	}
}

// --- GET /wallets/{walletId} -------------------------------------------------------

func TestReadingAWalletAnswers200(t *testing.T) {
	e := newWalletEnv(t)
	w := e.reader.result

	rec := e.do(http.MethodGet, "/wallets/"+w.ID().String(), "internal-token", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, fmt.Sprintf(`{"id":%q,"playerId":%q,"balance":{"amount":"100.00","currency":"BRL"},"version":1}`, w.ID(), w.PlayerID()), rec.Body.String())
	require.Equal(t, []uuid.UUID{w.ID()}, e.reader.calls)
}

func TestReadingAnUnknownWalletAnswers404(t *testing.T) {
	e := newWalletEnv(t)
	e.reader.err = application.ErrWalletNotFound
	rec := e.do(http.MethodGet, "/wallets/"+uuid.NewString(), "internal-token", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, CodeNotFound, decodeError(t, rec).Error.Code)
}

func TestReadingWithABadIdAnswers400(t *testing.T) {
	for _, id := range []string{"nope", "123", "7c9e6679742540de944be07fc1f90ae7", "00000000-0000-0000-0000-000000000000"} {
		e := newWalletEnv(t)
		rec := e.do(http.MethodGet, "/wallets/"+id, "internal-token", "")
		require.Equal(t, http.StatusBadRequest, rec.Code, id)
		require.Equal(t, CodeInvalidRequest, decodeError(t, rec).Error.Code)
		require.Empty(t, e.reader.calls)
	}
}

func TestReadFailuresAreMapped(t *testing.T) {
	e := newWalletEnv(t)
	e.reader.err = fmt.Errorf("%w: connection refused", application.ErrTemporarilyUnavailable)
	rec := e.do(http.MethodGet, "/wallets/"+uuid.NewString(), "internal-token", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))

	e.reader.err = errors.New("boom")
	rec = e.do(http.MethodGet, "/wallets/"+uuid.NewString(), "internal-token", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- who may call ------------------------------------------------------------------

func TestOnlyTheInternalRoleMayUseTheWalletEndpoints(t *testing.T) {
	id := uuid.NewString()
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/wallets", validBody(uuid.New())},
		{http.MethodGet, "/wallets/" + id, ""},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			e := newWalletEnv(t)

			rec := e.do(route.method, route.path, "", route.body)
			require.Equal(t, http.StatusUnauthorized, rec.Code, "no token")
			rec = e.do(route.method, route.path, "garbage", route.body)
			require.Equal(t, http.StatusUnauthorized, rec.Code, "a token that does not verify")
			rec = e.do(route.method, route.path, "provider-token", route.body)
			require.Equal(t, http.StatusForbidden, rec.Code, "a provider")

			require.Empty(t, e.opener.calls, "a denied request reaches no use case")
			require.Empty(t, e.reader.calls)
		})
	}
}

func TestTheWalletRoutesAreProtectedByTheInternalRoleOnly(t *testing.T) {
	e := newWalletEnv(t)
	got := map[string][]auth.Role{}
	for _, route := range e.routes.Registered() {
		require.False(t, route.Public, route.Pattern)
		got[route.Pattern] = route.Roles
	}
	require.Equal(t, map[string][]auth.Role{
		"POST /wallets":           {auth.RoleInternal},
		"GET /wallets/{walletId}": {auth.RoleInternal},
	}, got)
}

// --- correlation id ----------------------------------------------------------------

func TestTheCorrelationIdIsKeptWhenAcceptableAndGeneratedOtherwise(t *testing.T) {
	long := strings.Repeat("a", 129)
	cases := map[string]struct {
		headers  []string
		wantSame string // empty: a generated one is expected
	}{
		"given":            {[]string{"X-Correlation-Id", "req-42"}, "req-42"},
		"given at the cap": {[]string{"X-Correlation-Id", strings.Repeat("a", 128)}, strings.Repeat("a", 128)},
		"absent":           {nil, ""},
		"empty":            {[]string{"X-Correlation-Id", ""}, ""},
		"too long":         {[]string{"X-Correlation-Id", long}, ""},
		"with a space":     {[]string{"X-Correlation-Id", "two words"}, ""},
		"with a newline":   {[]string{"X-Correlation-Id", "a\nb"}, ""},
		"with DEL":         {[]string{"X-Correlation-Id", "a\x7fb"}, ""},
		"with a tilde":     {[]string{"X-Correlation-Id", "a~b"}, "a~b"},
		"non ascii":        {[]string{"X-Correlation-Id", "café"}, ""},
		"given twice":      {[]string{"X-Correlation-Id", "one", "X-Correlation-Id", "two"}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newWalletEnv(t)
			rec := e.do(http.MethodPost, "/wallets", "internal-token", validBody(uuid.New()), c.headers...)
			got := rec.Header().Get("X-Correlation-Id")
			require.NotEmpty(t, got)
			require.Equal(t, got, e.opener.calls[0].CorrelationID, "the use case gets what the client is told")
			if c.wantSame != "" {
				require.Equal(t, c.wantSame, got)
				return
			}
			parsed, err := uuid.Parse(got)
			require.NoError(t, err, "a generated id is a UUID")
			require.Equal(t, uuid.Version(7), parsed.Version())
		})
	}
}

func TestTheCorrelationIdAndTheWalletIdAreLogged(t *testing.T) {
	e := newWalletEnv(t)
	rec := e.do(http.MethodPost, "/wallets", "internal-token", validBody(uuid.New()), "X-Correlation-Id", "corr-77")
	require.Equal(t, http.StatusCreated, rec.Code)

	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(e.logs.String()), "\n") {
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &parsed))
		if parsed["msg"] == "wallet opened" {
			entry = parsed
		}
	}
	require.NotNil(t, entry, e.logs.String())
	require.Equal(t, "corr-77", entry["correlationId"])
	require.Equal(t, e.opener.result.ID().String(), entry["walletId"])
}
