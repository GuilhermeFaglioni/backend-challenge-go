package sqsconsumer

import "github.com/prometheus/client_golang/prometheus"

// How a message ended, the label of sqs_messages_handled_total.
const (
	resultHandled      = "handled"       // committed, then deleted
	resultDuplicate    = "duplicate"     // committed before (inbox or idempotency key), deleted
	resultRetried      = "retried"       // left in the queue to be received again, invisible for longer
	resultDeadLettered = "dead_lettered" // sent to the dead-letter queue and deleted
	resultReleased     = "released"      // given back untouched (shutdown, or held back by an earlier message of its group)
)

type metrics struct {
	received    prometheus.Counter
	handled     *prometheus.CounterVec
	deadLetters *prometheus.CounterVec
}

// newMetrics registers, every series at zero:
//
//	sqs_messages_received_total             messages taken from the queue (a message received again counts again)
//	sqs_messages_handled_total{result}      how each ended: handled, duplicate, retried, dead_lettered, released
//	sqs_dead_letters_total{reason}          messages sent to the dead-letter queue, by why
func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		received: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sqs_messages_received_total", Help: "Messages received from the input queue, a redelivery included.",
		}),
		handled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqs_messages_handled_total", Help: "Messages received from the input queue, by how they ended.",
		}, []string{"result"}),
		deadLetters: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sqs_dead_letters_total", Help: "Messages sent to the dead-letter queue by the consumer, by reason.",
		}, []string{"reason"}),
	}
	for _, r := range []string{resultHandled, resultDuplicate, resultRetried, resultDeadLettered, resultReleased} {
		m.handled.WithLabelValues(r)
	}
	for _, r := range Reasons {
		m.deadLetters.WithLabelValues(string(r))
	}
	if reg != nil {
		reg.MustRegister(m.received, m.handled, m.deadLetters)
	}
	return m
}
