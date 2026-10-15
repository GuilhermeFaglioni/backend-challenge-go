// Package retention removes, now and then, the inbox and outbox rows that have done their
// job, so that neither table grows forever. The database decides what may go: the role can
// only call the retention functions, which refuse anything younger than their floors.
package retention

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

// maxBatchesPerRun bounds one run, so that a large backlog is worked down over several runs
// instead of holding the worker for long.
const maxBatchesPerRun = 100

// Options configure a Worker.
type Options struct {
	Inbox      time.Duration // how long a completed message is remembered
	Outbox     time.Duration // how long a published event is kept
	Interval   time.Duration // time between runs
	BatchSize  int           // rows removed per statement
	Logger     *slog.Logger
	Registerer prometheus.Registerer
}

// Worker runs the retention.
type Worker struct {
	store   application.RetentionStore
	opts    Options
	log     *slog.Logger
	pruned  *prometheus.CounterVec
	errors  prometheus.Counter
	ctx     context.Context //nolint:containedctx // the lifetime of the runs
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	once    sync.Once
	started bool
	mu      sync.Mutex
}

// New builds a Worker and registers, at zero:
//
//	retention_pruned_total{table}   rows removed, by table (inbox, outbox)
//	retention_errors_total          runs of a table that failed
func New(store application.RetentionStore, opts Options) *Worker {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Registerer == nil {
		opts.Registerer = prometheus.NewRegistry()
	}
	w := &Worker{store: store, opts: opts, log: opts.Logger}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.pruned = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "retention_pruned_total", Help: "Inbox and outbox rows removed by the retention, by table.",
	}, []string{"table"})
	w.errors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "retention_errors_total", Help: "Retention runs of a table that failed.",
	})
	w.pruned.WithLabelValues("inbox")
	w.pruned.WithLabelValues("outbox")
	opts.Registerer.MustRegister(w.pruned, w.errors)
	return w
}

// Start runs the retention now and then every interval. Calling it again does nothing.
func (w *Worker) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.ctx.Err() != nil {
		return
	}
	w.started = true
	w.wg.Add(1)
	go w.loop()
}

// Stop ends the loop and waits for a run in progress, which stops between statements.
func (w *Worker) Stop(ctx context.Context) error {
	w.once.Do(func() { w.cancel() })
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) loop() {
	defer w.wg.Done()
	for {
		w.Run(w.ctx)
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(w.opts.Interval):
		}
	}
}

// Run removes what is old enough in both tables, a batch at a time.
func (w *Worker) Run(ctx context.Context) {
	w.prune(ctx, "inbox", func() (int, error) { return w.store.PruneInbox(ctx, w.opts.Inbox, w.opts.BatchSize) })
	w.prune(ctx, "outbox", func() (int, error) { return w.store.PruneOutbox(ctx, w.opts.Outbox, w.opts.BatchSize) })
}

func (w *Worker) prune(ctx context.Context, table string, batch func() (int, error)) {
	total := 0
	for range maxBatchesPerRun {
		if ctx.Err() != nil {
			break
		}
		removed, err := batch()
		if err != nil {
			if ctx.Err() == nil {
				w.errors.Inc()
				w.log.Warn("retention could not remove old rows", slog.String("table", table), slog.Any("error", err))
			}
			break
		}
		total += removed
		w.pruned.WithLabelValues(table).Add(float64(removed))
		if removed < w.opts.BatchSize {
			break
		}
	}
	if total > 0 {
		w.log.Info("retention removed old rows", slog.String("table", table), slog.Int("rows", total))
	}
}
