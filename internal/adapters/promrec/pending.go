package promrec

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/resolvepending"
)

// Pending measures the operations waiting for a Reference.
type Pending struct {
	resolutions *prometheus.CounterVec
	waiting     prometheus.Gauge
	oldest      prometheus.Gauge
}

var _ resolvepending.Recorder = (*Pending)(nil)

// NewPending registers, every series at zero:
//
//	pending_reference_resolutions_total{outcome}  each try of a waiting operation, by how it ended:
//	                                              processed, rejected, waiting (a retry), expired, discarded
//	pending_references                            operations waiting for a Reference right now
//	pending_reference_oldest_age_seconds          how long the one that has waited longest has waited
func NewPending(reg prometheus.Registerer) *Pending {
	p := &Pending{
		resolutions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pending_reference_resolutions_total", Help: "Tries of operations waiting for a Reference, by how they ended.",
		}, []string{"outcome"}),
		waiting: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pending_references", Help: "Operations waiting for a Reference.",
		}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pending_reference_oldest_age_seconds", Help: "How long the operation that has waited longest for its Reference has waited; 0 when none waits.",
		}),
	}
	for _, o := range resolvepending.Outcomes {
		p.resolutions.WithLabelValues(string(o))
	}
	reg.MustRegister(p.resolutions, p.waiting, p.oldest)
	return p
}

func (p *Pending) Resolved(o resolvepending.Outcome) {
	p.resolutions.WithLabelValues(string(o)).Inc()
}

// Observe sets the gauges from what the database holds.
func (p *Pending) Observe(s application.PendingStats) {
	p.waiting.Set(float64(s.Waiting))
	p.oldest.Set(s.Oldest.Seconds())
}
