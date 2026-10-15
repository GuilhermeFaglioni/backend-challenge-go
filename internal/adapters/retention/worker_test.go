package retention

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

type call struct {
	table     string
	olderThan time.Duration
	maxRows   int
}

type fakeStore struct {
	mu      sync.Mutex
	inbox   []int // what each call removes, in turn
	outbox  []int
	calls   []call
	failOut error
}

func (s *fakeStore) next(table string, queue *[]int, olderThan time.Duration, maxRows int) int {
	s.calls = append(s.calls, call{table, olderThan, maxRows})
	if len(*queue) == 0 {
		return 0
	}
	n := (*queue)[0]
	*queue = (*queue)[1:]
	return n
}

func (s *fakeStore) PruneInbox(_ context.Context, olderThan time.Duration, maxRows int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next("inbox", &s.inbox, olderThan, maxRows), nil
}

func (s *fakeStore) PruneOutbox(_ context.Context, olderThan time.Duration, maxRows int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOut != nil {
		s.calls = append(s.calls, call{"outbox", olderThan, maxRows})
		return 0, s.failOut
	}
	return s.next("outbox", &s.outbox, olderThan, maxRows), nil
}

func newWorker(store *fakeStore, reg *prometheus.Registry) *Worker {
	return New(store, Options{Inbox: 14 * 24 * time.Hour, Outbox: 7 * 24 * time.Hour, Interval: time.Hour, BatchSize: 100, Registerer: reg})
}

func TestARunRemovesBatchesUntilOneComesBackShort(t *testing.T) {
	store := &fakeStore{inbox: []int{100, 100, 30}, outbox: []int{7}}
	reg := prometheus.NewRegistry()
	w := newWorker(store, reg)

	w.Run(t.Context())

	require.Equal(t, []call{
		{"inbox", 14 * 24 * time.Hour, 100}, {"inbox", 14 * 24 * time.Hour, 100}, {"inbox", 14 * 24 * time.Hour, 100},
		{"outbox", 7 * 24 * time.Hour, 100},
	}, store.calls, "full batches mean there is more; a short one means done")
	require.InDelta(t, 230, testutil.ToFloat64(w.pruned.WithLabelValues("inbox")), 0)
	require.InDelta(t, 7, testutil.ToFloat64(w.pruned.WithLabelValues("outbox")), 0)
}

func TestARunIsBounded(t *testing.T) {
	full := make([]int, 500)
	for i := range full {
		full[i] = 100
	}
	store := &fakeStore{inbox: full}
	newWorker(store, prometheus.NewRegistry()).Run(t.Context())
	inbox := 0
	for _, c := range store.calls {
		if c.table == "inbox" {
			inbox++
		}
	}
	require.Equal(t, maxBatchesPerRun, inbox, "a large backlog is worked down over several runs")
}

func TestAFailingTableIsCountedAndDoesNotStopTheOther(t *testing.T) {
	store := &fakeStore{inbox: []int{3}, failOut: errors.New("down")}
	reg := prometheus.NewRegistry()
	w := newWorker(store, reg)

	w.Run(t.Context())

	require.InDelta(t, 3, testutil.ToFloat64(w.pruned.WithLabelValues("inbox")), 0)
	require.InDelta(t, 1, testutil.ToFloat64(w.errors), 0)
}

func TestTheWorkerRunsAtStartAndStopsCleanly(t *testing.T) {
	store := &fakeStore{inbox: []int{1}}
	w := newWorker(store, prometheus.NewRegistry())
	w.Start()
	w.Start()
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.calls) >= 2
	}, time.Second, time.Millisecond)
	require.NoError(t, w.Stop(t.Context()))
	require.NoError(t, w.Stop(t.Context()))
}

func TestMetricsStartAtZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	newWorker(&fakeStore{}, reg)
	require.Equal(t, 3, testutil.CollectAndCount(reg))
}
