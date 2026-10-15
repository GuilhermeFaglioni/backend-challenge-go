package promrec_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/promrec"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
)

func TestEveryOutcomeStartsAtZeroAndCountsItsOwn(t *testing.T) {
	reg := prometheus.NewRegistry()
	rec := promrec.NewOpenWallet(reg)

	count := func(outcome openwallet.Outcome) float64 {
		families, err := reg.Gather()
		require.NoError(t, err)
		for _, family := range families {
			if family.GetName() != "wallet_open_total" {
				continue
			}
			for _, metric := range family.GetMetric() {
				if metric.GetLabel()[0].GetValue() == string(outcome) {
					return metric.GetCounter().GetValue()
				}
			}
		}
		t.Fatalf("outcome %s has no series", outcome)
		return -1
	}

	for _, outcome := range openwallet.Outcomes {
		require.Zero(t, count(outcome), "%s must exist at zero before any request", outcome)
	}

	rec.Opened(openwallet.OutcomeCreated)
	rec.Opened(openwallet.OutcomeCreated)
	rec.Opened(openwallet.OutcomeConflict)
	require.Equal(t, 2.0, count(openwallet.OutcomeCreated))
	require.Equal(t, 1.0, count(openwallet.OutcomeConflict))
	require.Zero(t, count(openwallet.OutcomeInvalid))
	require.Zero(t, count(openwallet.OutcomeError))
	require.Equal(t, 4, testutil.CollectAndCount(reg, "wallet_open_total"))
}
