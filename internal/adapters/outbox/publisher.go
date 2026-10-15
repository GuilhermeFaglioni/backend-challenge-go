// Package outbox publishes the events written to the outbox table to the events
// queue (ARCHITECTURE.md, section 12). The write to the table is part of the database
// transaction that changed the wallet, so an event exists only if its change was
// committed; this worker is the separate process that gets it out afterwards.
//
// Delivery is at-least-once. The worker reserves a batch under a lease, sends it, and
// then marks it published; if it dies in between, the lease runs out and another
// instance sends the same events again with the same event ids, which is how a
// consumer (and the queue's deduplication) recognises them.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
)

const (
	baseBackoff = time.Second
	maxBackoff  = 5 * time.Minute
	// jitterSpread is how far a backoff may stray from its nominal value, so
	// publishers that failed together do not retry together.
	jitterSpread = 0.2
	// defaultLagEvery is how often the lag gauge asks the database.
	defaultLagEvery = 2 * time.Second
	// releaseTimeout bounds giving the leases back when the stop deadline has passed.
	releaseTimeout = 5 * time.Second
)

// Options configures a Publisher.
type Options struct {
	PollInterval time.Duration // wait when a cycle found nothing
	BatchSize    int           // wallets reserved together; each round sends one event of each, so at most 10 (one SendMessageBatch)
	PerWallet    int           // consecutive events of a wallet reserved at once, sent one per round; default 1
	Lease        time.Duration // how long a reserved event stays reserved
	Logger       *slog.Logger
	Registerer   prometheus.Registerer

	// LagEvery is how often the lag gauge is refreshed; defaults to 2s.
	LagEvery time.Duration
	// Jitter returns a number in [0,1); for tests. Defaults to a random one.
	Jitter func() float64
}

// Publisher is the worker. Start launches its loop; Stop ends it.
type Publisher struct {
	store  application.OutboxStore
	sender application.MessageSender
	opts   Options
	log    *slog.Logger

	metrics metrics

	// workCtx bounds the calls of the loop. It is cancelled only when a stop has run
	// out of time, so a graceful stop lets the batch in flight finish.
	workCtx    context.Context
	cancelWork context.CancelFunc

	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{} // closed when the loop has ended

	mu       sync.Mutex
	state    state
	inFlight []uuid.UUID // reserved and not yet recorded; released if a stop runs out of time
	lastLag  time.Time
}

type state int

const (
	stateNew state = iota
	stateRunning
	stateStopped
)

type metrics struct {
	published prometheus.Counter
	failures  prometheus.Counter
	retries   prometheus.Counter
	lag       prometheus.Gauge
	reserveEr prometheus.Counter
}

// New builds a Publisher. Metrics are registered on opts.Registerer, all at zero.
func New(store application.OutboxStore, sender application.MessageSender, opts Options) *Publisher {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Registerer == nil {
		opts.Registerer = prometheus.NewRegistry()
	}
	if opts.LagEvery <= 0 {
		opts.LagEvery = defaultLagEvery
	}
	if opts.Jitter == nil {
		opts.Jitter = rand.Float64 //nolint:gosec // spreading retries needs no secure randomness
	}
	p := &Publisher{store: store, sender: sender, opts: opts, log: opts.Logger, stopping: make(chan struct{}), done: make(chan struct{})}
	p.workCtx, p.cancelWork = context.WithCancel(context.Background())

	m := metrics{
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_publish_failures_total", Help: "Attempts to publish an outbox event that failed.",
		}),
		retries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_publish_retries_total", Help: "Attempts to publish an outbox event that had already failed before.",
		}),
		lag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_lag_seconds", Help: "Age of the oldest outbox event not yet published; 0 when there is none.",
		}),
		reserveEr: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_reserve_errors_total", Help: "Times the publisher could not reserve a batch.",
		}),
	}
	m.published = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "outbox_published_total", Help: "Outbox events accepted by the events queue and recorded as published.",
	})
	opts.Registerer.MustRegister(m.published, m.failures, m.retries, m.lag, m.reserveEr)
	p.metrics = m
	return p
}

// Start launches the publishing loop. Calling it again, or after Stop, does nothing.
func (p *Publisher) Start() {
	p.mu.Lock()
	if p.state != stateNew {
		p.mu.Unlock()
		return
	}
	p.state = stateRunning
	p.mu.Unlock()

	p.log.Info("outbox publisher started",
		slog.Duration("pollInterval", p.opts.PollInterval), slog.Int("batchSize", p.opts.BatchSize), slog.Duration("lease", p.opts.Lease))
	go p.loop()
}

// Stop ends the loop. It lets the batch in flight finish and be recorded; if ctx
// ends first, it interrupts the work and gives the leases back so another instance
// can take the events at once. Stopping twice, or a publisher never started, is fine.
func (p *Publisher) Stop(ctx context.Context) error {
	p.mu.Lock()
	wasRunning := p.state == stateRunning
	p.state = stateStopped
	p.mu.Unlock()
	p.stopOnce.Do(func() { close(p.stopping) })

	if wasRunning {
		select {
		case <-p.done:
		case <-ctx.Done():
			p.cancelWork()
			<-p.done
		}
		p.log.Info("outbox publisher stopped")
	}
	p.cancelWork()

	p.mu.Lock()
	leftover := p.inFlight
	p.inFlight = nil
	p.mu.Unlock()
	if len(leftover) > 0 {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := p.store.Release(releaseCtx, leftover); err != nil {
			p.log.Error("could not release the outbox leases; they run out by themselves", slog.Any("error", err), slog.Int("events", len(leftover)))
		} else {
			p.log.Info("released the outbox events still in flight", slog.Int("events", len(leftover)))
		}
	}
	return nil
}

func (p *Publisher) loop() {
	defer close(p.done)
	for {
		select {
		case <-p.stopping:
			return
		default:
		}
		// Having found work means there is probably more, and the next event of a wallet
		// can only be sent once the one before it is recorded, so a publisher that waited
		// the poll interval after each cycle would send one event per wallet per interval.
		// It waits only when a cycle found nothing.
		if p.cycle(p.workCtx) > 0 {
			continue
		}
		select {
		case <-p.stopping:
			return
		case <-time.After(p.opts.PollInterval):
		}
	}
}

// cycle reserves, sends and records one batch. It returns how many events it handled.
func (p *Publisher) cycle(ctx context.Context) int {
	p.updateLag(ctx)

	entries, err := p.store.Reserve(ctx, p.opts.BatchSize, max(p.opts.PerWallet, 1), p.opts.Lease)
	if err != nil {
		p.metrics.reserveEr.Inc()
		p.log.Warn("could not reserve outbox events", slog.Any("error", err))
		return 0
	}
	if len(entries) == 0 {
		return 0
	}
	p.track(entries)
	faultinject.Hit(faultinject.OutboxAfterReserve)

	sent, failures, unsent := p.publishInRounds(ctx, entries)
	faultinject.Hit(faultinject.OutboxAfterPublish)

	p.record(ctx, sent, failures)
	// A batch interrupted by a stop that ran out of time stays tracked: Stop gives
	// its leases back. Releasing after the outcome was recorded is harmless.
	if ctx.Err() == nil {
		if len(unsent) > 0 {
			if err := p.store.Release(ctx, unsent); err != nil {
				p.log.Warn("could not release outbox events held back by an earlier one that failed; their lease runs out by itself",
					slog.Any("error", err), slog.Int("events", len(unsent)))
			}
		}
		p.untrack()
	}
	return len(entries)
}

// publishInRounds sends the reserved events so that a wallet's order holds even when the
// queue takes only part of a request (SQS reports each entry of a SendMessageBatch on its
// own, and does not promise that a refused entry stops the later ones of its group). Each
// round sends at most one event per wallet, all wallets together in one request; a wallet's
// next event goes in the next round, and only if the one before it was accepted. A wallet
// whose event failed stops there: the failed event is retried with backoff, and the ones
// after it are given back unsent, to go out after it.
func (p *Publisher) publishInRounds(ctx context.Context, entries []application.OutboxEntry) (sent []application.OutboundMessage, failures []application.OutboxFailure, unsent []uuid.UUID) {
	var order []uuid.UUID
	chains := map[uuid.UUID][]application.OutboxEntry{}
	for _, entry := range entries { // in the order they were written
		if _, seen := chains[entry.AggregateID]; !seen {
			order = append(order, entry.AggregateID)
		}
		chains[entry.AggregateID] = append(chains[entry.AggregateID], entry)
	}
	stoppedAt := map[uuid.UUID]int{} // wallet -> index of the event that failed

	for round := 0; ctx.Err() == nil; round++ {
		var batch []application.OutboxEntry
		for _, wallet := range order {
			if _, stopped := stoppedAt[wallet]; !stopped && round < len(chains[wallet]) {
				batch = append(batch, chains[wallet][round])
			}
		}
		if len(batch) == 0 {
			break
		}
		messages, failed := p.prepare(batch)
		if len(messages) > 0 {
			var accepted []application.OutboundMessage
			accepted, failed = p.send(ctx, batch, messages, failed)
			if ctx.Err() != nil {
				// Interrupted by a stop that ran out of time: what was accepted is still
				// recorded; nothing else is counted, and Stop gives the leases back.
				return append(sent, accepted...), failures, nil
			}
			sent = append(sent, accepted...)
		}
		for _, f := range failed {
			for _, entry := range batch {
				if entry.EventID == f.EventID {
					stoppedAt[entry.AggregateID] = round
				}
			}
		}
		failures = append(failures, failed...)
	}

	for wallet, at := range stoppedAt {
		for _, entry := range chains[wallet][at+1:] {
			unsent = append(unsent, entry.EventID)
		}
	}
	return sent, failures, unsent
}

// prepare builds the message of each entry. An entry that cannot be serialized (a
// corrupt row) fails on its own, and does not hold back the others.
func (p *Publisher) prepare(entries []application.OutboxEntry) ([]application.OutboundMessage, []application.OutboxFailure) {
	messages := make([]application.OutboundMessage, 0, len(entries))
	var failures []application.OutboxFailure
	for _, entry := range entries {
		if entry.Attempts > 0 {
			p.metrics.retries.Inc()
		}
		body, err := json.Marshal(event.Envelope{
			EventID: entry.EventID, EventType: entry.EventType, AggregateID: entry.AggregateID,
			CorrelationID: entry.CorrelationID, CausationID: entry.CausationID,
			OccurredAt: entry.OccurredAt, Version: entry.Version, Data: entry.Payload,
		})
		if err != nil {
			p.log.Error("outbox event cannot be serialized", slog.String("eventId", entry.EventID.String()), slog.Any("error", err))
			failures = append(failures, p.failure(entry, fmt.Sprintf("serialize: %v", err)))
			continue
		}
		messages = append(messages, application.OutboundMessage{
			EventID: entry.EventID, GroupID: entry.AggregateID.String(), DeduplicationID: entry.EventID.String(),
			EventType: entry.EventType, CorrelationID: entry.CorrelationID, Body: body,
		})
	}
	return messages, failures
}

// send hands the messages to the queue and sorts the outcome: what the queue accepted,
// and what failed (the whole batch, or just the refused entries).
func (p *Publisher) send(ctx context.Context, entries []application.OutboxEntry, messages []application.OutboundMessage, failures []application.OutboxFailure) ([]application.OutboundMessage, []application.OutboxFailure) {
	byID := make(map[uuid.UUID]application.OutboxEntry, len(entries))
	for _, entry := range entries {
		byID[entry.EventID] = entry
	}

	refused, err := p.sender.Send(ctx, messages)
	if err != nil {
		if ctx.Err() != nil {
			// A stop that ran out of time interrupted the send. That is not the
			// event's fault: no attempt is counted, and Stop gives the leases back.
			return nil, failures
		}
		p.log.Warn("could not publish a batch of outbox events", slog.Any("error", err), slog.Int("events", len(messages)))
		for _, message := range messages {
			failures = append(failures, p.failure(byID[message.EventID], err.Error()))
		}
		return nil, failures
	}

	var accepted []application.OutboundMessage
	for _, message := range messages {
		if reason, isRefused := refused[message.EventID]; isRefused {
			p.log.Warn("the queue refused an outbox event", slog.String("eventId", message.EventID.String()), slog.Any("error", reason))
			failures = append(failures, p.failure(byID[message.EventID], reason.Error()))
			continue
		}
		accepted = append(accepted, message)
	}
	return accepted, failures
}

// record writes the outcome down. A failure to do so is logged and left to the lease:
// the events were sent, so they will be sent again, which at-least-once allows.
func (p *Publisher) record(ctx context.Context, sent []application.OutboundMessage, failures []application.OutboxFailure) {
	if len(sent) > 0 {
		ids := make([]uuid.UUID, len(sent))
		for i, message := range sent {
			ids[i] = message.EventID
		}
		if err := p.store.MarkPublished(ctx, ids); err != nil {
			p.log.Error("sent outbox events but could not record them as published; they will be sent again when their lease runs out",
				slog.Any("error", err), slog.Int("events", len(ids)))
		} else {
			p.metrics.published.Add(float64(len(ids)))
			eventIDs := make([]string, len(ids))
			for i, id := range ids {
				eventIDs[i] = id.String()
			}
			p.log.Info("outbox events published", slog.Int("events", len(ids)), slog.Any("eventIds", eventIDs))
		}
	}
	if len(failures) > 0 {
		p.metrics.failures.Add(float64(len(failures)))
		if err := p.store.MarkFailed(ctx, failures); err != nil {
			p.log.Error("could not record failed outbox publications; they will be retried when their lease runs out",
				slog.Any("error", err), slog.Int("events", len(failures)))
		}
	}
}

func (p *Publisher) failure(entry application.OutboxEntry, reason string) application.OutboxFailure {
	return application.OutboxFailure{EventID: entry.EventID, Reason: reason, RetryIn: backoff(entry.Attempts+1, p.opts.Jitter)}
}

func (p *Publisher) track(entries []application.OutboxEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight = p.inFlight[:0]
	for _, entry := range entries {
		p.inFlight = append(p.inFlight, entry.EventID)
	}
}

func (p *Publisher) untrack() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight = nil
}

// updateLag refreshes the lag gauge, at most every LagEvery.
func (p *Publisher) updateLag(ctx context.Context) {
	p.mu.Lock()
	due := p.lastLag.IsZero() || time.Since(p.lastLag) >= p.opts.LagEvery
	if due {
		p.lastLag = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	age, any, err := p.store.OldestUnpublished(ctx)
	if err != nil {
		p.log.Warn("could not read the outbox lag", slog.Any("error", err))
		return
	}
	if !any {
		p.metrics.lag.Set(0)
		return
	}
	p.metrics.lag.Set(age.Seconds())
}

// backoff is the wait before the next attempt, after the attempt-th failure: one
// second, doubling, up to five minutes, spread by up to 20% either way. The cap is
// applied in seconds, before converting to a Duration, so a huge attempt count cannot
// overflow the conversion.
func backoff(attempt int, jitter func() float64) time.Duration {
	seconds := min(baseBackoff.Seconds()*math.Pow(2, float64(max(attempt-1, 0))), maxBackoff.Seconds())
	spread := 1 + jitterSpread*(2*jitter()-1)
	return time.Duration(seconds * spread * float64(time.Second))
}
