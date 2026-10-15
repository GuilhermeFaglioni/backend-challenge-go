package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

// scopedReader behaves like the real reader: what is outside the scope is not found.
type scopedReader struct {
	transactions []wager.Transaction
	err          error
	calls        []string
	scopes       []application.Scope
}

func (r *scopedReader) ByID(_ context.Context, id uuid.UUID, scope application.Scope) (wager.Transaction, error) {
	r.calls = append(r.calls, "id:"+id.String())
	r.scopes = append(r.scopes, scope)
	if r.err != nil {
		return wager.Transaction{}, r.err
	}
	for _, t := range r.transactions {
		if t.ID() == id && scope.Reaches(t.External().ProviderID) {
			return t, nil
		}
	}
	return wager.Transaction{}, application.ErrTransactionNotFound
}

func (r *scopedReader) ByExternalID(_ context.Context, provider, externalID string) (wager.Transaction, error) {
	r.calls = append(r.calls, "ext:"+provider+"/"+externalID)
	if r.err != nil {
		return wager.Transaction{}, r.err
	}
	for _, t := range r.transactions {
		if t.External().ProviderID == provider && t.External().ExternalTransactionID == externalID {
			return t, nil
		}
	}
	return wager.Transaction{}, application.ErrTransactionNotFound
}

type txFixture struct {
	processed, rejected, waiting, failed, otherProviders, opening wager.Transaction
}

func brl(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	return m
}

func accepted(t *testing.T, provider, externalID string, kind wager.Kind, reference string) wager.Transaction {
	t.Helper()
	request, err := wager.NewRequest(wager.RequestParams{
		ProviderID: provider, ExternalTransactionID: externalID, PlayerID: wagerPlayer, WalletID: wagerWallet,
		RoundID: "round-987", GameID: "fortune-chimp", Kind: kind, Amount: brl(t, 2500), ReferenceExternalTransactionID: reference,
	})
	require.NoError(t, err)
	tx, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request, IdempotencyKey: "key-" + externalID, CorrelationID: "corr-1", At: now})
	require.NoError(t, err)
	return tx
}

func newTxFixture(t *testing.T) txFixture {
	t.Helper()
	later := now.Add(time.Second)
	processed, err := accepted(t, "provider-a", "tx-processed", wager.KindBet, "").Process(wager.Result{Balance: brl(t, 97500), WalletVersion: 3}, later)
	require.NoError(t, err)
	rejected, err := accepted(t, "provider-a", "tx-rejected", wager.KindBet, "").Reject(wager.FailureInsufficientFunds, later)
	require.NoError(t, err)
	waiting, err := accepted(t, "provider-a", "tx-waiting", wager.KindRefund, "tx-processed").AwaitReference(later)
	require.NoError(t, err)
	failed, err := accepted(t, "provider-a", "tx-failed", wager.KindBet, "").Fail(later)
	require.NoError(t, err)
	other, err := accepted(t, "provider-b", "tx-other", wager.KindBet, "").Process(wager.Result{Balance: brl(t, 1), WalletVersion: 2}, later)
	require.NoError(t, err)

	pending, err := wager.NewOpening(wager.OpeningParams{ID: uuid.New(), WalletID: wagerWallet, PlayerID: wagerPlayer, Amount: brl(t, 100000), CorrelationID: "c", At: now})
	require.NoError(t, err)
	opening, err := pending.Process(wager.Result{Balance: brl(t, 100000), WalletVersion: 1}, later)
	require.NoError(t, err)
	return txFixture{processed: processed, rejected: rejected, waiting: waiting, failed: failed, otherProviders: other, opening: opening}
}

type txEnv struct {
	fx       txFixture
	reader   *scopedReader
	routes   *Routes
	registry *prometheus.Registry
	logs     *bytes.Buffer
}

func newTxEnv(t *testing.T) *txEnv {
	t.Helper()
	fx := newTxFixture(t)
	e := &txEnv{fx: fx, logs: &bytes.Buffer{}, registry: prometheus.NewRegistry(), reader: &scopedReader{transactions: []wager.Transaction{fx.processed, fx.rejected, fx.waiting, fx.failed, fx.otherProviders, fx.opening}}}
	logger := slog.New(slog.NewJSONHandler(e.logs, nil))
	e.routes = NewRoutes(fakeAuthenticator{
		"provider-a-token": {principal: providerA},
		"provider-b-token": {principal: providerB},
		"internal-token":   {principal: internal},
	}, logger, e.registry)
	NewTransactions(e.reader, logger).register(e.routes)
	return e
}

func (e *txEnv) get(token, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.routes.Handler().ServeHTTP(rec, req)
	return rec
}

func byID(tx wager.Transaction) string { return "/wagering/transactions/" + tx.ID().String() }

// --- what a transaction looks like ---------------------------------------------------

func TestAProcessedTransactionShowsItsBalance(t *testing.T) {
	e := newTxEnv(t)
	rec := e.get("provider-a-token", byID(e.fx.processed))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, fmt.Sprintf(`{"transactionId":%q,"status":"PROCESSED","kind":"BET","providerId":"provider-a","externalTransactionId":"tx-processed",
		"playerId":%q,"walletId":%q,"roundId":"round-987","gameId":"fortune-chimp","money":{"amount":"25.00","currency":"BRL"},
		"balance":{"amount":"975.00","currency":"BRL"},"createdAt":"2026-10-14T12:00:00.000Z","updatedAt":"2026-10-14T12:00:01.000Z","processedAt":"2026-10-14T12:00:01.000Z"}`,
		e.fx.processed.ID(), wagerPlayer, wagerWallet), rec.Body.String())
}

func TestARejectionShowsItsFailureCodeAndNoBalance(t *testing.T) {
	e := newTxEnv(t)
	rec := e.get("provider-a-token", byID(e.fx.rejected))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"REJECTED"`)
	require.Contains(t, rec.Body.String(), `"failureCode":"INSUFFICIENT_FUNDS"`)
	require.NotContains(t, rec.Body.String(), `"balance"`)
}

func TestATransactionWaitingForItsReferenceShowsThatState(t *testing.T) {
	e := newTxEnv(t)
	rec := e.get("provider-a-token", byID(e.fx.waiting))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, `"status":"PENDING_REFERENCE"`)
	require.Contains(t, body, `"kind":"REFUND"`)
	require.Contains(t, body, `"referenceExternalTransactionId":"tx-processed"`)
	require.NotContains(t, body, `"processedAt"`, "it has not ended")
	require.NotContains(t, body, `"balance"`)
	require.NotContains(t, body, `"failureCode"`)
}

func TestAFailureShowsItsCode(t *testing.T) {
	e := newTxEnv(t)
	rec := e.get("provider-a-token", byID(e.fx.failed))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"FAILED"`)
	require.Contains(t, rec.Body.String(), `"failureCode":"INTERNAL_PERMANENT_FAILURE"`)
}

func TestTheViewNeverShowsTheKeyOrTheHash(t *testing.T) {
	e := newTxEnv(t)
	body := e.get("provider-a-token", byID(e.fx.processed)).Body.String()
	require.NotContains(t, body, "key-tx-processed")
	require.NotContains(t, body, string(e.fx.processed.External().PayloadHash))
	require.NotContains(t, body, "corr-1", "the correlation id is the writer's, not part of the result")
}

func TestAnUnknownWalletRejectionShowsTheWalletThatWasAskedFor(t *testing.T) {
	e := newTxEnv(t)
	missing, err := accepted(t, "provider-a", "tx-ghost", wager.KindBet, "").Reject(wager.FailureWalletNotFound, now.Add(time.Second))
	require.NoError(t, err)
	e.reader.transactions = append(e.reader.transactions, missing)
	rec := e.get("provider-a-token", byID(missing))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"walletId":"`+wagerWallet.String()+`"`)
}

// --- isolation -----------------------------------------------------------------------

// Asking for what exists but is not yours must look exactly like asking for what does
// not exist: otherwise the difference tells who has which transactions.
func TestAnotherProvidersTransactionIsIndistinguishableFromOneThatDoesNotExist(t *testing.T) {
	e := newTxEnv(t)

	theirs := e.get("provider-a-token", byID(e.fx.otherProviders))
	nonexistent := e.get("provider-a-token", "/wagering/transactions/"+uuid.NewString())
	require.Equal(t, http.StatusNotFound, theirs.Code)
	require.Equal(t, http.StatusNotFound, nonexistent.Code)
	require.Equal(t, nonexistent.Body.String(), theirs.Body.String(), "the same body")
	require.Equal(t, CodeNotFound, decodeError(t, theirs).Error.Code)
	for _, header := range []string{"Content-Type", "Retry-After", "WWW-Authenticate"} {
		require.Equal(t, nonexistent.Header().Get(header), theirs.Header().Get(header), header)
	}

	// The same for the lookup by external id: provider A asking under its own name for
	// the external id of a transaction that belongs to provider B.
	ext := e.get("provider-a-token", "/providers/provider-a/wagering/transactions/tx-other")
	none := e.get("provider-a-token", "/providers/provider-a/wagering/transactions/tx-nothing-like-it")
	require.Equal(t, http.StatusNotFound, ext.Code)
	require.Equal(t, none.Body.String(), ext.Body.String())
}

func TestAProviderCannotReadTheInternalOpeningOfAWallet(t *testing.T) {
	e := newTxEnv(t)
	rec := e.get("provider-a-token", byID(e.fx.opening))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestTheScopeFollowsTheRole(t *testing.T) {
	e := newTxEnv(t)
	e.get("provider-a-token", byID(e.fx.processed))
	e.get("internal-token", byID(e.fx.processed))
	require.Equal(t, application.ProviderTransactions("provider-a"), e.reader.scopes[0], "a provider's own, from the token")
	require.Equal(t, application.AllTransactions(), e.reader.scopes[1])
}

func TestTheInternalServiceReadsAnyTransactionIncludingAnOpening(t *testing.T) {
	e := newTxEnv(t)
	for _, tx := range []wager.Transaction{e.fx.processed, e.fx.otherProviders, e.fx.opening} {
		rec := e.get("internal-token", byID(tx))
		require.Equal(t, http.StatusOK, rec.Code, tx.ID())
	}
	opening := e.get("internal-token", byID(e.fx.opening)).Body.String()
	require.Contains(t, opening, `"kind":"OPENING"`)
	require.NotContains(t, opening, "providerId", "an internal opening has no provider")
	require.NotContains(t, opening, "externalTransactionId")
	require.NotContains(t, opening, "roundId")
}

func TestAProviderMustAskUnderItsOwnName(t *testing.T) {
	e := newTxEnv(t)

	own := e.get("provider-a-token", "/providers/provider-a/wagering/transactions/tx-processed")
	require.Equal(t, http.StatusOK, own.Code, own.Body.String())

	e.reader.calls = nil
	other := e.get("provider-a-token", "/providers/provider-b/wagering/transactions/tx-other")
	require.Equal(t, http.StatusForbidden, other.Code, "another provider's namespace is not yours to read")
	require.Equal(t, CodeForbidden, decodeError(t, other).Error.Code)
	require.Empty(t, e.reader.calls, "and nothing is read for a request that is not allowed")
	require.InDelta(t, 1, failureCount(t, e.registry, auth.ReasonProviderMismatch), 0, "it is counted as a provider acting for another")
}

func TestTheInternalServiceMayAskUnderAnyProvider(t *testing.T) {
	e := newTxEnv(t)
	for provider, external := range map[string]string{"provider-a": "tx-processed", "provider-b": "tx-other"} {
		rec := e.get("internal-token", "/providers/"+provider+"/wagering/transactions/"+external)
		require.Equal(t, http.StatusOK, rec.Code, provider)
	}
	rec := e.get("internal-token", "/providers/provider-a/wagering/transactions/nope")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAnExternalIdIsFoundOnlyUnderItsOwnProvider(t *testing.T) {
	e := newTxEnv(t)
	rec := e.get("internal-token", "/providers/provider-a/wagering/transactions/tx-other")
	require.Equal(t, http.StatusNotFound, rec.Code, "tx-other belongs to provider-b")
}

// --- who may call --------------------------------------------------------------------

func TestOnlyAuthenticatedCallersWithARoleCanRead(t *testing.T) {
	e := newTxEnv(t)
	for _, path := range []string{byID(e.fx.processed), "/providers/provider-a/wagering/transactions/tx-processed"} {
		require.Equal(t, http.StatusUnauthorized, e.get("", path).Code)
		require.Equal(t, http.StatusUnauthorized, e.get("garbage", path).Code)
	}
	require.Empty(t, e.reader.calls)
}

func TestTheReadRoutesAreOpenToProvidersAndTheInternalService(t *testing.T) {
	e := newTxEnv(t)
	got := map[string][]auth.Role{}
	for _, route := range e.routes.Registered() {
		got[route.Pattern] = route.Roles
	}
	both := []auth.Role{auth.RoleProvider, auth.RoleInternal}
	require.Equal(t, map[string][]auth.Role{
		"GET /wagering/transactions/{transactionId}":                                both,
		"GET /providers/{providerId}/wagering/transactions/{externalTransactionId}": both,
	}, got)
}

// --- errors --------------------------------------------------------------------------

func TestABadTransactionIdIs400(t *testing.T) {
	e := newTxEnv(t)
	for _, id := range []string{"nope", "123", "3fa85f6457174562b3fc2c963f66afa6", "00000000-0000-0000-0000-000000000000"} {
		rec := e.get("provider-a-token", "/wagering/transactions/"+id)
		require.Equal(t, http.StatusBadRequest, rec.Code, id)
		require.Equal(t, CodeInvalidRequest, decodeError(t, rec).Error.Code)
	}
	require.Empty(t, e.reader.calls)
}

func TestReadFailuresAreMappedWithoutLeakingInternals(t *testing.T) {
	secret := "password=hunter2 host=10.0.0.5"
	e := newTxEnv(t)

	e.reader.err = fmt.Errorf("%w: %s", application.ErrTemporarilyUnavailable, secret)
	rec := e.get("provider-a-token", byID(e.fx.processed))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.NotContains(t, rec.Body.String(), "hunter2")

	e.reader.err = errors.New(secret)
	rec = e.get("provider-a-token", byID(e.fx.processed), "X-Correlation-Id", "corr-read-1")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "hunter2")
	require.Contains(t, e.logs.String(), "hunter2", "the cause is in the log")
	require.Contains(t, e.logs.String(), "corr-read-1")
}

func TestTheCorrelationIdIsReturned(t *testing.T) {
	e := newTxEnv(t)
	rec := e.get("provider-a-token", byID(e.fx.processed), "X-Correlation-Id", "corr-77")
	require.Equal(t, "corr-77", rec.Header().Get("X-Correlation-Id"))
	rec = e.get("provider-a-token", byID(e.fx.processed))
	_, err := uuid.Parse(rec.Header().Get("X-Correlation-Id"))
	require.NoError(t, err)
}
