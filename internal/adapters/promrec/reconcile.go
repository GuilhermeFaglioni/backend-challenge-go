package promrec

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/reconcile"
)

// Reconcile counts the results of reconciliations.
type Reconcile struct {
	runs        *prometheus.CounterVec
	divergences prometheus.Counter
}

var _ reconcile.Recorder = (*Reconcile)(nil)

// NewReconcile registers, at zero:
//
//	reconciliations_total{result}          the checks made, by result (consistent, divergent)
//	reconciliation_divergences_total       the checks that found the books and the balance apart
func NewReconcile(reg prometheus.Registerer) *Reconcile {
	r := &Reconcile{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reconciliations_total", Help: "Wallet reconciliations made, by result.",
		}, []string{"result"}),
		divergences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reconciliation_divergences_total", Help: "Reconciliations that found a stored balance different from the ledger's.",
		}),
	}
	r.runs.WithLabelValues("consistent")
	r.runs.WithLabelValues("divergent")
	reg.MustRegister(r.runs, r.divergences)
	return r
}

func (r *Reconcile) Reconciled(result reconcile.Result) {
	if result.Consistent {
		r.runs.WithLabelValues("consistent").Inc()
		return
	}
	r.runs.WithLabelValues("divergent").Inc()
	r.divergences.Inc()
}
