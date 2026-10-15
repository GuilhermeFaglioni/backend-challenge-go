// Package promrec implements the metric recorders the use cases depend on, on top of
// Prometheus. The use cases know only the small interfaces they declare.
package promrec

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
)

// OpenWallet counts the outcomes of opening a wallet in wallet_open_total{outcome}.
type OpenWallet struct {
	opened *prometheus.CounterVec
}

var _ openwallet.Recorder = (*OpenWallet)(nil)

// NewOpenWallet registers the counter on reg, with every outcome starting at zero so
// the series exist before the first request.
func NewOpenWallet(reg prometheus.Registerer) *OpenWallet {
	opened := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wallet_open_total",
		Help: "Attempts to open a wallet, by outcome.",
	}, []string{"outcome"})
	for _, outcome := range openwallet.Outcomes {
		opened.WithLabelValues(string(outcome))
	}
	reg.MustRegister(opened)
	return &OpenWallet{opened: opened}
}

func (o *OpenWallet) Opened(outcome openwallet.Outcome) {
	o.opened.WithLabelValues(string(outcome)).Inc()
}
