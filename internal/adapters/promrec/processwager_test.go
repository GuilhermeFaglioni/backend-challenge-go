package promrec_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/promrec"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

func series(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (value float64, found bool) {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	metrics:
		for _, m := range family.GetMetric() {
			for _, l := range m.GetLabel() {
				if want, ok := labels[l.GetName()]; ok && want != l.GetValue() {
					continue metrics
				}
			}
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}

func TestEverySeriesStartsAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	promrec.NewWager(reg)
	for _, kind := range []string{"BET", "WIN", "LOSS", "REFUND", "ROLLBACK", "unknown"} {
		for _, outcome := range processwager.Outcomes {
			v, found := series(t, reg, "wager_requests_total", map[string]string{"kind": kind, "outcome": string(outcome)})
			require.True(t, found, "%s/%s has no series", kind, outcome)
			require.Zero(t, v)
		}
		_, found := series(t, reg, "wager_processing_duration_seconds", map[string]string{"kind": kind})
		require.True(t, found, kind)
	}
	for _, d := range []string{"replay", "key_conflict", "external_id"} {
		_, found := series(t, reg, "wager_duplicates_total", map[string]string{"type": d})
		require.True(t, found, d)
	}
	for _, r := range application.RetryReasons {
		_, found := series(t, reg, "db_transaction_retries_total", map[string]string{"reason": string(r)})
		require.True(t, found, r)
	}
}

func TestEachRequestIsCountedUnderItsKindAndOutcomeAndTimed(t *testing.T) {
	reg := prometheus.NewRegistry()
	w := promrec.NewWager(reg)
	w.Handled(wager.KindBet, processwager.OutcomeProcessed, 5*time.Millisecond)
	w.Handled(wager.KindBet, processwager.OutcomeProcessed, 7*time.Millisecond)
	w.Handled(wager.KindBet, processwager.OutcomeRejected, time.Millisecond)

	v, _ := series(t, reg, "wager_requests_total", map[string]string{"kind": "BET", "outcome": "processed"})
	require.Equal(t, 2.0, v)
	v, _ = series(t, reg, "wager_requests_total", map[string]string{"kind": "BET", "outcome": "rejected"})
	require.Equal(t, 1.0, v)
	v, _ = series(t, reg, "wager_processing_duration_seconds", map[string]string{"kind": "BET"})
	require.Equal(t, 3.0, v, "every request is timed, whatever its outcome")
}

func TestDuplicatesAreCountedByWhatTheyRepeated(t *testing.T) {
	reg := prometheus.NewRegistry()
	w := promrec.NewWager(reg)
	w.Handled(wager.KindBet, processwager.OutcomeReplayed, 0)
	w.Handled(wager.KindBet, processwager.OutcomeReplayed, 0)
	w.Handled(wager.KindBet, processwager.OutcomeKeyConflict, 0)
	w.Handled(wager.KindBet, processwager.OutcomeDuplicateExternal, 0)
	w.Handled(wager.KindBet, processwager.OutcomeProcessed, 0)

	for typ, want := range map[string]float64{"replay": 2, "key_conflict": 1, "external_id": 1} {
		v, _ := series(t, reg, "wager_duplicates_total", map[string]string{"type": typ})
		require.Equal(t, want, v, typ)
	}
}

// The kind comes from the caller: a made-up one must not create a series.
func TestAKindThatIsNotOursNeverBecomesALabel(t *testing.T) {
	reg := prometheus.NewRegistry()
	w := promrec.NewWager(reg)
	w.Handled(wager.Kind("DEFINITELY-NOT-A-KIND-"+time.Now().String()), processwager.OutcomeInvalid, 0)
	w.Handled(wager.KindOpening, processwager.OutcomeInvalid, 0)

	v, _ := series(t, reg, "wager_requests_total", map[string]string{"kind": "unknown", "outcome": "invalid"})
	require.Equal(t, 2.0, v)
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "wager_requests_total" {
			continue
		}
		require.Len(t, family.GetMetric(), 6*len(processwager.Outcomes), "no series beyond the known kinds and 'unknown'")
	}
}

func TestRetriesAreCountedByReason(t *testing.T) {
	reg := prometheus.NewRegistry()
	w := promrec.NewWager(reg)
	w.Retried(application.RetryDeadlock)
	w.Retried(application.RetryDeadlock)
	w.Retried(application.RetryConcurrentUpdate)
	for reason, want := range map[application.RetryReason]float64{
		application.RetryDeadlock: 2, application.RetryConcurrentUpdate: 1, application.RetrySerializationFailure: 0,
	} {
		v, _ := series(t, reg, "db_transaction_retries_total", map[string]string{"reason": string(reason)})
		require.Equal(t, want, v, reason)
	}
}
