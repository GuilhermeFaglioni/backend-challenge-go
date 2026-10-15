package promrec_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/promrec"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/reconcile"
)

func TestDivergencesAreCountedAndHealthyChecksAreNot(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := promrec.NewReconcile(reg)

	for _, labels := range []map[string]string{{"result": "consistent"}, {"result": "divergent"}} {
		v, found := series(t, reg, "reconciliations_total", labels)
		require.True(t, found, labels)
		require.Zero(t, v, "every series starts at zero")
	}
	v, found := series(t, reg, "reconciliation_divergences_total", map[string]string{})
	require.True(t, found)
	require.Zero(t, v)

	r.Reconciled(reconcile.Result{Consistent: true})
	r.Reconciled(reconcile.Result{Consistent: true})
	r.Reconciled(reconcile.Result{Consistent: false})

	v, _ = series(t, reg, "reconciliations_total", map[string]string{"result": "consistent"})
	require.Equal(t, 2.0, v)
	v, _ = series(t, reg, "reconciliations_total", map[string]string{"result": "divergent"})
	require.Equal(t, 1.0, v)
	v, _ = series(t, reg, "reconciliation_divergences_total", map[string]string{})
	require.Equal(t, 1.0, v, "only the divergent one")
}
