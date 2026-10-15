package promrec

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

// unknownKind labels a request whose kind is not one of ours. The kind comes from
// outside, so it is never used as a label as it is: that would let a caller make the
// number of series grow without bound.
const unknownKind = "unknown"

var wagerKinds = []wager.Kind{wager.KindBet, wager.KindWin, wager.KindLoss, wager.KindRefund, wager.KindRollback}

// Wager measures the processing of external operations.
type Wager struct {
	requests   *prometheus.CounterVec
	duplicates *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	retries    *prometheus.CounterVec
}

var (
	_ processwager.Recorder     = (*Wager)(nil)
	_ application.RetryObserver = (*Wager)(nil)
)

// NewWager registers the metrics on reg, every series at zero:
//
//	wager_requests_total{kind,outcome}      every request, by how it ended
//	wager_duplicates_total{type}            replays, key conflicts and duplicate external ids
//	wager_processing_duration_seconds{kind} how long a request took, whatever the outcome
//	db_transaction_retries_total{reason}    concurrency conflicts that a retry absorbed
func NewWager(reg prometheus.Registerer) *Wager {
	w := &Wager{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_requests_total", Help: "External operations handled, by kind and outcome.",
		}, []string{"kind", "outcome"}),
		duplicates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_duplicates_total", Help: "Operations that repeated one already seen, by what they repeated.",
		}, []string{"type"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "wager_processing_duration_seconds", Help: "Time to handle an external operation.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
		}, []string{"kind"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "db_transaction_retries_total", Help: "Units of work retried after a concurrency conflict, by reason.",
		}, []string{"reason"}),
	}
	kinds := append([]string{unknownKind}, kindNames()...)
	for _, kind := range kinds {
		for _, outcome := range processwager.Outcomes {
			w.requests.WithLabelValues(kind, string(outcome))
		}
		w.duration.WithLabelValues(kind)
	}
	for _, t := range []string{duplicateReplay, duplicateKeyConflict, duplicateExternal} {
		w.duplicates.WithLabelValues(t)
	}
	for _, reason := range application.RetryReasons {
		w.retries.WithLabelValues(string(reason))
	}
	reg.MustRegister(w.requests, w.duplicates, w.duration, w.retries)
	return w
}

const (
	duplicateReplay      = "replay"
	duplicateKeyConflict = "key_conflict"
	duplicateExternal    = "external_id"
)

func kindNames() []string {
	names := make([]string, len(wagerKinds))
	for i, k := range wagerKinds {
		names[i] = string(k)
	}
	return names
}

func kindLabel(kind wager.Kind) string {
	for _, k := range wagerKinds {
		if k == kind {
			return string(k)
		}
	}
	return unknownKind
}

// Handled records one request.
func (w *Wager) Handled(kind wager.Kind, outcome processwager.Outcome, took time.Duration) {
	label := kindLabel(kind)
	w.requests.WithLabelValues(label, string(outcome)).Inc()
	w.duration.WithLabelValues(label).Observe(took.Seconds())
	switch outcome {
	case processwager.OutcomeReplayed:
		w.duplicates.WithLabelValues(duplicateReplay).Inc()
	case processwager.OutcomeKeyConflict:
		w.duplicates.WithLabelValues(duplicateKeyConflict).Inc()
	case processwager.OutcomeDuplicateExternal:
		w.duplicates.WithLabelValues(duplicateExternal).Inc()
	}
}

// Retried records a unit of work that ran again after a concurrency conflict.
func (w *Wager) Retried(reason application.RetryReason) {
	w.retries.WithLabelValues(string(reason)).Inc()
}
