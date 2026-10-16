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

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/reconcile"
)

type fakeReconciler struct {
	result reconcile.Result
	err    error
	calls  []uuid.UUID
	modes  []reconcile.Mode
}

func (f *fakeReconciler) Execute(_ context.Context, id uuid.UUID, mode reconcile.Mode) (reconcile.Result, error) {
	f.calls = append(f.calls, id)
	f.modes = append(f.modes, mode)
	return f.result, f.err
}

func newReconciliationEnv(t *testing.T) (*Routes, *fakeReconciler, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	fake := &fakeReconciler{result: reconcile.Result{
		WalletID: id, Stored: brl(t, 7500), Calculated: brl(t, 7500), Difference: brl(t, 0), Consistent: true, CheckedEntries: 2, ReadEntries: 2,
	}}
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	routes := NewRoutes(fakeAuthenticator{
		"internal-token": {principal: internal},
		"provider-token": {principal: providerA},
	}, logger, prometheus.NewRegistry())
	NewReconciliation(fake, logger).register(routes)
	return routes, fake, id
}

func reconcileRequest(routes *Routes, token, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	routes.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAConsistentWalletAnswers200WithTheBooks(t *testing.T) {
	routes, fake, id := newReconciliationEnv(t)
	rec := reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, fmt.Sprintf(`{"walletId":%q,"storedBalance":{"amount":"75.00","currency":"BRL"},
		"calculatedBalance":{"amount":"75.00","currency":"BRL"},"difference":{"amount":"0.00","currency":"BRL"},
		"consistent":true,"checkedEntries":2,"mode":"full","readEntries":2}`, id), rec.Body.String())
	require.Equal(t, []uuid.UUID{id}, fake.calls)
	require.Equal(t, []reconcile.Mode{reconcile.Full}, fake.modes, "the whole ledger unless asked otherwise")
}

func TestTheModeChoosesTheCheckAndAFullOneReportsTheCheckpoint(t *testing.T) {
	for query, want := range map[string]reconcile.Mode{"?mode=full": reconcile.Full, "?mode=incremental": reconcile.Incremental, "": reconcile.Full} {
		routes, fake, id := newReconciliationEnv(t)
		rec := reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation"+query)
		require.Equal(t, http.StatusOK, rec.Code, query)
		require.Equal(t, []reconcile.Mode{want}, fake.modes, query)
	}

	routes, fake, id := newReconciliationEnv(t)
	fake.result.Mode, fake.result.ReadEntries = reconcile.Incremental, 0
	rec := reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation?mode=incremental")
	require.Contains(t, rec.Body.String(), `"mode":"incremental"`)
	require.Contains(t, rec.Body.String(), `"readEntries":0`)

	fake.result.Mode, fake.result.ReadEntries, fake.result.CheckpointChecked = reconcile.Full, 2, true
	fake.result.Consistent, fake.result.CheckpointMismatch = false, true
	rec = reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation")
	require.Contains(t, rec.Body.String(), `"mode":"full"`)
	require.Contains(t, rec.Body.String(), `"readEntries":2`)
	require.Contains(t, rec.Body.String(), `"checkpointMatches":false`)

	fake.result.CheckpointMismatch = false
	rec = reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation")
	require.Contains(t, rec.Body.String(), `"checkpointMatches":true`)

	fake.result.CheckpointChecked = false
	rec = reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation")
	require.NotContains(t, rec.Body.String(), "checkpointMatches", "no checkpoint, nothing to say about one")
}

func TestAModeThatIsNotKnownIs400AndChecksNothing(t *testing.T) {
	for _, query := range []string{"?mode=yes", "?mode=", "?mode=FULL", "?mode=full&mode=full", "?mode=incremental&mode=full"} {
		routes, fake, id := newReconciliationEnv(t)
		rec := reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation"+query)
		require.Equal(t, http.StatusBadRequest, rec.Code, query)
		require.Contains(t, rec.Body.String(), `"field":"mode"`, query)
		require.Empty(t, fake.calls, query)
	}
}

func TestADivergenceIsStill200AndShowsTheSignedDifference(t *testing.T) {
	routes, fake, id := newReconciliationEnv(t)
	fake.result = reconcile.Result{
		WalletID: id, Stored: brl(t, 7200), Calculated: brl(t, 7500), Difference: brl(t, -300), Consistent: false, CheckedEntries: 2,
	}
	rec := reconcileRequest(routes, "internal-token", "/wallets/"+id.String()+"/reconciliation")
	require.Equal(t, http.StatusOK, rec.Code, "a finding is not a failure of the request")
	require.Contains(t, rec.Body.String(), `"difference":{"amount":"-3.00","currency":"BRL"}`)
	require.Contains(t, rec.Body.String(), `"consistent":false`)
}

func TestReconciliationRefusesWhatItShould(t *testing.T) {
	routes, fake, id := newReconciliationEnv(t)
	path := "/wallets/" + id.String() + "/reconciliation"

	require.Equal(t, http.StatusForbidden, reconcileRequest(routes, "provider-token", path).Code, "internal role only")
	require.Equal(t, http.StatusUnauthorized, reconcileRequest(routes, "", path).Code)
	require.Equal(t, http.StatusBadRequest, reconcileRequest(routes, "internal-token", "/wallets/nope/reconciliation").Code)
	require.Empty(t, fake.calls, "none of those reached the use case")

	fake.err = application.ErrWalletNotFound
	rec := reconcileRequest(routes, "internal-token", path)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, CodeNotFound, decodeError(t, rec).Error.Code)
}

func TestReconciliationFailuresAreMappedWithoutLeakingInternals(t *testing.T) {
	routes, fake, id := newReconciliationEnv(t)
	path := "/wallets/" + id.String() + "/reconciliation"

	fake.err = fmt.Errorf("%w: password=hunter2", application.ErrTemporarilyUnavailable)
	rec := reconcileRequest(routes, "internal-token", path)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.NotContains(t, rec.Body.String(), "hunter2")

	fake.err = errors.New("password=hunter2")
	rec = reconcileRequest(routes, "internal-token", path)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "hunter2")
}

func TestReconciliationIsRegisteredForTheInternalRoleOnly(t *testing.T) {
	routes, _, _ := newReconciliationEnv(t)
	got := routes.Registered()
	require.Len(t, got, 1)
	require.Equal(t, "POST /wallets/{walletId}/reconciliation", got[0].Pattern)
	require.Len(t, got[0].Roles, 1)
}
