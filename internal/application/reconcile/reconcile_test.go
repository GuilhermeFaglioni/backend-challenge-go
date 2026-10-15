package reconcile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/reconcile"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

type fakeAuditor struct {
	audit application.LedgerAudit
	err   error
	calls []uuid.UUID
}

func (f *fakeAuditor) Audit(_ context.Context, id uuid.UUID) (application.LedgerAudit, error) {
	f.calls = append(f.calls, id)
	return f.audit, f.err
}

type recorder struct{ results []reconcile.Result }

func (r *recorder) Reconciled(result reconcile.Result) { r.results = append(r.results, result) }

type fixture struct {
	auditor *fakeAuditor
	rec     *recorder
	logs    *bytes.Buffer
	uc      *reconcile.UseCase
}

func newFixture(credits, debits, stored int64, entries int64) *fixture {
	f := &fixture{
		auditor: &fakeAuditor{audit: application.LedgerAudit{
			Currency: money.BRL, Stored: stored, Credits: big.NewInt(credits), Debits: big.NewInt(debits), Entries: entries,
		}},
		rec: &recorder{}, logs: &bytes.Buffer{},
	}
	f.uc = reconcile.New(reconcile.Deps{Auditor: f.auditor, Recorder: f.rec, Logger: slog.New(slog.NewJSONHandler(f.logs, nil))})
	return f
}

func TestAConsistentWalletIsReportedAsSuch(t *testing.T) {
	f := newFixture(10000, 2500, 7500, 2)
	id := uuid.New()

	got, err := f.uc.Execute(t.Context(), id)
	require.NoError(t, err)

	require.Equal(t, id, got.WalletID)
	require.Equal(t, int64(7500), got.Stored.Minor())
	require.Equal(t, int64(7500), got.Calculated.Minor(), "credits minus debits")
	require.Equal(t, int64(0), got.Difference.Minor())
	require.Equal(t, money.BRL, got.Difference.Currency())
	require.True(t, got.Consistent)
	require.Equal(t, int64(2), got.CheckedEntries)
	require.Equal(t, []uuid.UUID{id}, f.auditor.calls)
	require.Len(t, f.rec.results, 1)
	require.True(t, f.rec.results[0].Consistent)
	require.NotContains(t, f.logs.String(), `"level":"ERROR"`, "a healthy wallet logs no error")
}

func TestADivergenceIsTheStoredBalanceMinusTheCalculatedOne(t *testing.T) {
	for name, c := range map[string]struct {
		stored, wantDifference int64
	}{
		"more stored than the ledger explains": {8000, 500},
		"less stored than the ledger explains": {7200, -300},
		"a negative stored balance, corrupted": {-100, -7600},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(10000, 2500, c.stored, 2)
			got, err := f.uc.Execute(t.Context(), uuid.New())
			require.NoError(t, err, "a divergence is a result, not an error")
			require.False(t, got.Consistent)
			require.Equal(t, c.wantDifference, got.Difference.Minor())
			require.Equal(t, int64(7500), got.Calculated.Minor())
			require.Equal(t, c.stored, got.Stored.Minor())
		})
	}
}

func TestADivergenceIsLoggedAsAnErrorWithTheWalletAndTheDifference(t *testing.T) {
	f := newFixture(10000, 2500, 8000, 2)
	id := uuid.New()
	_, err := f.uc.Execute(t.Context(), id)
	require.NoError(t, err)

	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &parsed))
		if parsed["level"] == "ERROR" {
			entry = parsed
		}
	}
	require.NotNil(t, entry, f.logs.String())
	require.Equal(t, id.String(), entry["walletId"])
	require.Equal(t, "5.00", entry["difference"])
	require.Equal(t, "reconciliation found a divergence", entry["msg"])
	require.False(t, f.rec.results[0].Consistent, "and the recorder is told")
}

func TestTheDifferenceOfExtremeBooksDoesNotOverflow(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 70) // more than any int64
	f := newFixture(0, 0, math.MaxInt64, 1)
	f.auditor.audit.Credits = huge
	f.auditor.audit.Debits = big.NewInt(0)
	_, err := f.uc.Execute(t.Context(), uuid.New())
	require.ErrorIs(t, err, reconcile.ErrBooksOutOfRange, "a ledger that adds up to more than a balance can hold is reported, not wrapped around")
}

func TestAWalletWithNothingInItsLedgerHasACalculatedBalanceOfZero(t *testing.T) {
	f := newFixture(0, 0, 0, 0)
	got, err := f.uc.Execute(t.Context(), uuid.New())
	require.NoError(t, err)
	require.True(t, got.Consistent)
	require.Zero(t, got.CheckedEntries)
}

func TestErrorsAreReportedAsTheyAre(t *testing.T) {
	f := newFixture(0, 0, 0, 0)
	f.auditor.err = fmt.Errorf("wrapped: %w", application.ErrWalletNotFound)
	_, err := f.uc.Execute(t.Context(), uuid.New())
	require.ErrorIs(t, err, application.ErrWalletNotFound)
	require.Empty(t, f.rec.results, "no result to record")

	boom := errors.New("boom")
	f.auditor.err = boom
	_, err = f.uc.Execute(t.Context(), uuid.New())
	require.ErrorIs(t, err, boom)
}
