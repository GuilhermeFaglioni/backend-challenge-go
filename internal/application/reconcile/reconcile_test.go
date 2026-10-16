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
	audit       application.LedgerAudit
	err         error
	calls       []uuid.UUID
	full        []bool
	recorded    []int64 // the seqs checkpoints were asked for
	recordedFor []uuid.UUID
	recordErr   error
}

func (f *fakeAuditor) Audit(_ context.Context, id uuid.UUID, full bool) (application.LedgerAudit, error) {
	f.calls = append(f.calls, id)
	f.full = append(f.full, full)
	return f.audit, f.err
}

func (f *fakeAuditor) RecordCheckpoint(_ context.Context, id uuid.UUID, throughSeq int64) (bool, error) {
	f.recorded = append(f.recorded, throughSeq)
	f.recordedFor = append(f.recordedFor, id)
	return f.recordErr == nil, f.recordErr
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

	got, err := f.uc.Execute(t.Context(), id, reconcile.Incremental)
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
			got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
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
	_, err := f.uc.Execute(t.Context(), id, reconcile.Incremental)
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
	_, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
	require.ErrorIs(t, err, reconcile.ErrBooksOutOfRange, "a ledger that adds up to more than a balance can hold is reported, not wrapped around")
}

func TestAWalletWithNothingInItsLedgerHasACalculatedBalanceOfZero(t *testing.T) {
	f := newFixture(0, 0, 0, 0)
	got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
	require.NoError(t, err)
	require.True(t, got.Consistent)
	require.Zero(t, got.CheckedEntries)
}

func TestErrorsAreReportedAsTheyAre(t *testing.T) {
	f := newFixture(0, 0, 0, 0)
	f.auditor.err = fmt.Errorf("wrapped: %w", application.ErrWalletNotFound)
	_, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
	require.ErrorIs(t, err, application.ErrWalletNotFound)
	require.Empty(t, f.rec.results, "no result to record")

	boom := errors.New("boom")
	f.auditor.err = boom
	_, err = f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
	require.ErrorIs(t, err, boom)
}

func sums(credits, debits, entries int64) application.LedgerSums {
	return application.LedgerSums{Credits: big.NewInt(credits), Debits: big.NewInt(debits), Entries: entries}
}

func TestAnIncrementalCheckAddsTheEntriesReadToTheCheckpoint(t *testing.T) {
	// Checkpoint: 100.00 in, 25.00 out over 2 entries, through seq 40. Read after it: a
	// 10.00 bet. Stored: 65.00.
	f := newFixture(0, 1000, 6500, 1)
	f.auditor.audit.Base = &application.LedgerCheckpoint{ThroughSeq: 40, LedgerSums: sums(10000, 2500, 2)}
	f.auditor.audit.LastSeq = 57
	id := uuid.New()

	got, err := f.uc.Execute(t.Context(), id, reconcile.Incremental)
	require.NoError(t, err)
	require.True(t, got.Consistent)
	require.Equal(t, int64(6500), got.Calculated.Minor())
	require.Equal(t, int64(3), got.CheckedEntries, "the checkpoint's entries and the one read")
	require.Equal(t, int64(1), got.ReadEntries)
	require.Equal(t, reconcile.Incremental, got.Mode)
	require.Equal(t, []bool{false}, f.auditor.full)
	require.Equal(t, []int64{57}, f.auditor.recorded, "consistent: a checkpoint through the last entry")
	require.Equal(t, []uuid.UUID{id}, f.auditor.recordedFor)
}

func TestACheckpointIsRecordedOnlyForConsistentBooksWithNewEntries(t *testing.T) {
	t.Run("divergent", func(t *testing.T) {
		f := newFixture(10000, 2500, 8000, 2)
		f.auditor.audit.LastSeq = 9
		_, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
		require.NoError(t, err)
		require.Empty(t, f.auditor.recorded, "a divergence is never checkpointed")
	})
	t.Run("nothing after the checkpoint", func(t *testing.T) {
		f := newFixture(0, 0, 7500, 0)
		f.auditor.audit.Base = &application.LedgerCheckpoint{ThroughSeq: 40, LedgerSums: sums(10000, 2500, 2)}
		f.auditor.audit.LastSeq = 40
		got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
		require.NoError(t, err)
		require.True(t, got.Consistent)
		require.Empty(t, f.auditor.recorded)
	})
	t.Run("a wallet without entries", func(t *testing.T) {
		f := newFixture(0, 0, 0, 0)
		got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
		require.NoError(t, err)
		require.True(t, got.Consistent)
		require.Empty(t, f.auditor.recorded)
	})
}

func TestAFailureToRecordTheCheckpointDoesNotChangeTheResult(t *testing.T) {
	f := newFixture(10000, 2500, 7500, 2)
	f.auditor.audit.LastSeq = 9
	f.auditor.recordErr = errors.New("database gone")
	got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Incremental)
	require.NoError(t, err)
	require.True(t, got.Consistent)
	require.Contains(t, f.logs.String(), "could not record the reconciliation checkpoint")
	require.Len(t, f.rec.results, 1)
}

func TestAFullCheckReadsEverythingAndChecksTheCheckpoint(t *testing.T) {
	full := func(prefix application.LedgerSums) *fixture {
		f := newFixture(10000, 3500, 6500, 3)
		f.auditor.audit.Latest = &application.LedgerCheckpoint{ThroughSeq: 40, LedgerSums: sums(10000, 2500, 2)}
		f.auditor.audit.Prefix = &prefix
		f.auditor.audit.LastSeq = 57
		return f
	}

	t.Run("the checkpoint agrees with the ledger", func(t *testing.T) {
		f := full(sums(10000, 2500, 2))
		got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Full)
		require.NoError(t, err)
		require.True(t, got.Consistent)
		require.False(t, got.CheckpointMismatch)
		require.True(t, got.CheckpointChecked)
		require.Equal(t, reconcile.Full, got.Mode)
		require.Equal(t, int64(3), got.CheckedEntries, "the checkpoint is not added: everything was read")
		require.Equal(t, int64(3), got.ReadEntries)
		require.Equal(t, int64(6500), got.Calculated.Minor())
		require.Equal(t, []bool{true}, f.auditor.full)
		require.Equal(t, []int64{57}, f.auditor.recorded)
	})
	for name, prefix := range map[string]application.LedgerSums{
		"other credits": sums(10001, 2500, 2),
		"other debits":  sums(10000, 2499, 2),
		"other count":   sums(10000, 2500, 3),
	} {
		t.Run("the checkpoint disagrees: "+name, func(t *testing.T) {
			f := full(prefix)
			got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Full)
			require.NoError(t, err)
			require.False(t, got.Consistent, "a checkpoint that lies is a finding, even when the balance matches")
			require.True(t, got.CheckpointMismatch)
			require.True(t, got.Difference.IsZero())
			require.Empty(t, f.auditor.recorded)
			require.Contains(t, f.logs.String(), "reconciliation found a checkpoint that does not match the ledger")
			require.NotContains(t, f.logs.String(), "reconciliation found a divergence", "the balance itself agrees")
		})
	}
	t.Run("nothing after the checkpoint", func(t *testing.T) {
		f := newFixture(10000, 2500, 7500, 2)
		f.auditor.audit.Latest = &application.LedgerCheckpoint{ThroughSeq: 40, LedgerSums: sums(10000, 2500, 2)}
		f.auditor.audit.Prefix = &application.LedgerSums{Credits: big.NewInt(10000), Debits: big.NewInt(2500), Entries: 2}
		f.auditor.audit.LastSeq = 40
		got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Full)
		require.NoError(t, err)
		require.True(t, got.Consistent)
		require.Empty(t, f.auditor.recorded, "the checkpoint already covers every entry")
	})
	t.Run("no checkpoint yet", func(t *testing.T) {
		f := newFixture(10000, 2500, 7500, 2)
		f.auditor.audit.LastSeq = 9
		got, err := f.uc.Execute(t.Context(), uuid.New(), reconcile.Full)
		require.NoError(t, err)
		require.True(t, got.Consistent)
		require.False(t, got.CheckpointChecked)
		require.Equal(t, []int64{9}, f.auditor.recorded)
	})
}
