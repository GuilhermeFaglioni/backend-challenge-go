// Package pendingref is the worker that takes up again the operations waiting for a
// Reference that had not arrived (ARCHITECTURE.md, section 10). The state lives in the
// database, so the worker keeps none: any instance may reserve what is due, a reservation
// is a lease that runs out if its holder dies, and each operation is tried in a database
// transaction of its own (resolvepending).
package pendingref

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/resolvepending"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
)

// releaseTimeout bounds giving the leases back when the stop deadline has passed.
const releaseTimeout = 5 * time.Second

// Resolver makes one try for an operation: the resolvepending use case.
type Resolver interface {
	Execute(ctx context.Context, item application.PendingItem) (resolvepending.Result, error)
}

// StatsObserver is told how many operations are waiting and for how long.
type StatsObserver interface {
	Observe(application.PendingStats)
}

// Options configures a Worker.
type Options struct {
	PollInterval time.Duration // wait when a cycle found nothing
	BatchSize    int           // operations reserved together
	Lease        time.Duration // how long a reserved operation stays reserved
	StatsEvery   time.Duration // how often the gauges are refreshed; default 2s
	Logger       *slog.Logger
	Observer     StatsObserver // optional

	// MaxErrors is how many tries failing for a reason other than an outage an operation
	// may have before it ends as FAILED. Zero means it is never failed.
	MaxErrors int
	// ErrorBackoffBase and ErrorBackoffMax hold an operation back after an error: the base
	// after the first, doubling with each one, up to the maximum.
	ErrorBackoffBase time.Duration
	ErrorBackoffMax  time.Duration
	// Recorder is told about the tries that failed and the operations that were failed.
	Recorder resolvepending.Recorder // optional
}

// Worker is the loop. Start launches it; Stop ends it.
type Worker struct {
	store    application.PendingStore
	resolver Resolver
	opts     Options
	log      *slog.Logger

	// workCtx bounds the tries. It is cancelled only when a stop has run out of time, so
	// a graceful stop lets the try in hand finish.
	workCtx    context.Context
	cancelWork context.CancelFunc

	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{}

	mu       sync.Mutex
	started  bool
	stopped  bool
	reserved map[uuid.UUID]bool // reserved and not done; released if a stop runs out of time
}

// New builds a Worker.
func New(store application.PendingStore, resolver Resolver, opts Options) *Worker {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.StatsEvery <= 0 {
		opts.StatsEvery = 2 * time.Second
	}
	w := &Worker{
		store: store, resolver: resolver, opts: opts, log: opts.Logger,
		stopping: make(chan struct{}), done: make(chan struct{}), reserved: map[uuid.UUID]bool{},
	}
	w.workCtx, w.cancelWork = context.WithCancel(context.Background())
	return w
}

// Start launches the loop. Calling it again, or after Stop, does nothing.
func (w *Worker) Start() {
	w.mu.Lock()
	if w.started || w.stopped {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()
	w.log.Info("pending reference worker started",
		slog.Duration("pollInterval", w.opts.PollInterval), slog.Int("batchSize", w.opts.BatchSize), slog.Duration("lease", w.opts.Lease))
	go w.loop()
}

// Stop ends the loop. It lets the try in hand finish; if ctx ends first, it interrupts it
// and gives the leases back so another instance can take the operations at once.
// Stopping twice, or a worker never started, is fine.
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	wasRunning := w.started && !w.stopped
	w.stopped = true
	w.mu.Unlock()
	w.stopOnce.Do(func() { close(w.stopping) })

	if wasRunning {
		select {
		case <-w.done:
		case <-ctx.Done():
			w.cancelWork()
			<-w.done
		}
		w.log.Info("pending reference worker stopped")
	}
	w.cancelWork()

	w.mu.Lock()
	var leftover []uuid.UUID
	for id := range w.reserved {
		leftover = append(leftover, id)
	}
	w.reserved = map[uuid.UUID]bool{}
	w.mu.Unlock()
	if len(leftover) > 0 {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := w.store.Release(releaseCtx, leftover); err != nil {
			w.log.Error("could not release the pending operations; their leases run out by themselves", slog.Any("error", err), slog.Int("operations", len(leftover)))
		}
	}
	return nil
}

func (w *Worker) loop() {
	defer close(w.done)
	lastStats := time.Time{}
	for {
		select {
		case <-w.stopping:
			return
		default:
		}
		if time.Since(lastStats) >= w.opts.StatsEvery {
			w.observe(w.workCtx)
			lastStats = time.Now()
		}
		if w.cycle(w.workCtx) >= w.opts.BatchSize {
			continue // a full batch: there is probably more
		}
		select {
		case <-w.stopping:
			return
		case <-time.After(w.opts.PollInterval):
		}
	}
}

func (w *Worker) observe(ctx context.Context) {
	if w.opts.Observer == nil {
		return
	}
	stats, err := w.store.Stats(ctx)
	if err != nil {
		w.log.Warn("could not count the pending operations", slog.Any("error", err))
		return
	}
	w.opts.Observer.Observe(stats)
}

// cycle reserves one batch and tries each operation in it. It returns how many it took.
func (w *Worker) cycle(ctx context.Context) int {
	items, err := w.store.Reserve(ctx, w.opts.BatchSize, w.opts.Lease)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("could not reserve pending operations", slog.Any("error", err))
		}
		return 0
	}
	if len(items) == 0 {
		return 0
	}
	w.mu.Lock()
	// The previous batch is over. What it could not do keeps its lease, which runs out by
	// itself; only the batch in hand is given back by a stop that runs out of time.
	w.reserved = map[uuid.UUID]bool{}
	for _, item := range items {
		w.reserved[item.TransactionID] = true
	}
	w.mu.Unlock()
	// The operations are leased and nothing has been tried: another instance must take
	// them when the lease runs out.
	faultinject.Hit(faultinject.PendingAfterReserve)

	for _, item := range items {
		select {
		case <-w.stopping:
			return len(items) // the rest keep their leases untouched; Stop releases them
		default:
		}
		_, err := w.resolver.Execute(ctx, item)
		if err != nil {
			if ctx.Err() == nil {
				w.failedTry(ctx, item, err)
			}
			continue
		}
		w.mu.Lock()
		delete(w.reserved, item.TransactionID)
		w.mu.Unlock()
	}
	return len(items)
}

// failedTry deals with a try that wrote nothing. An outage is not the operation's fault: the
// lease keeps it away from the other workers for a while, which is also the pause before
// it is tried again, and nothing is counted. Anything else is a defect that trying again
// may never fix: it is counted, and an operation that keeps failing ends as FAILED, so that
// it is recorded for audit instead of being tried forever.
func (w *Worker) failedTry(ctx context.Context, item application.PendingItem, cause error) {
	id := slog.String("transactionId", item.TransactionID.String())
	if errors.Is(cause, application.ErrTemporarilyUnavailable) || w.opts.MaxErrors < 1 {
		w.log.Warn("could not try a pending operation; it will be tried again when its lease runs out", id, slog.Any("error", cause))
		return
	}
	base, ceiling := w.opts.ErrorBackoffBase, w.opts.ErrorBackoffMax
	if base <= 0 {
		base = w.opts.Lease
	}
	ceiling = max(ceiling, base)
	count, err := w.store.RecordError(ctx, item.TransactionID, cause.Error(), base, ceiling)
	if err != nil {
		w.log.Warn("could not record the error of a pending operation", id, slog.Any("error", err), slog.Any("cause", cause))
		return
	}
	if count == 0 {
		return // decided meanwhile
	}
	w.record(resolvepending.OutcomeError)
	if count < w.opts.MaxErrors {
		w.log.Warn("a pending operation could not be tried; it will be tried again",
			id, slog.Int("errors", count), slog.Int("maxErrors", w.opts.MaxErrors), slog.Any("error", cause))
		return
	}
	failed, err := w.store.Fail(ctx, item.TransactionID)
	if err != nil {
		w.log.Error("could not fail a pending operation that keeps failing", id, slog.Any("error", err))
		return
	}
	if failed {
		w.record(resolvepending.OutcomeFailed)
		w.log.Error("a pending operation kept failing and was ended as FAILED (INTERNAL_PERMANENT_FAILURE)",
			id, slog.Int("errors", count), slog.Any("error", cause))
	}
}

func (w *Worker) record(o resolvepending.Outcome) {
	if w.opts.Recorder != nil {
		w.opts.Recorder.Resolved(o)
	}
}
