package promrec_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/promrec"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/resolvepending"
)

func TestPendingStartsEverySeriesAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	promrec.NewPending(reg)
	require.Equal(t, len(resolvepending.Outcomes)+2, testutil.CollectAndCount(reg))
}

func TestPendingCountsEachTryByOutcomeAndShowsWhatWaits(t *testing.T) {
	reg := prometheus.NewRegistry()
	p := promrec.NewPending(reg)
	for _, o := range []resolvepending.Outcome{
		resolvepending.OutcomeWaiting, resolvepending.OutcomeWaiting, resolvepending.OutcomeWaiting,
		resolvepending.OutcomeProcessed, resolvepending.OutcomeExpired, resolvepending.OutcomeRejected, resolvepending.OutcomeDiscarded,
	} {
		p.Resolved(o)
	}
	p.Observe(application.PendingStats{Waiting: 5, Oldest: 90 * time.Second})

	families, err := reg.Gather()
	require.NoError(t, err)
	got := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			name := f.GetName()
			if len(m.GetLabel()) > 0 {
				name += "{" + m.GetLabel()[0].GetValue() + "}"
			}
			if m.GetCounter() != nil {
				got[name] = m.GetCounter().GetValue()
			} else {
				got[name] = m.GetGauge().GetValue()
			}
		}
	}
	require.Equal(t, map[string]float64{
		"pending_reference_resolutions_total{waiting}":   3,
		"pending_reference_resolutions_total{processed}": 1,
		"pending_reference_resolutions_total{expired}":   1,
		"pending_reference_resolutions_total{rejected}":  1,
		"pending_reference_resolutions_total{discarded}": 1,
		"pending_references":                             5,
		"pending_reference_oldest_age_seconds":           90,
	}, got)
}
