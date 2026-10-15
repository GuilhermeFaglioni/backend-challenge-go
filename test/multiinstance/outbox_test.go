//go:build multiinstance

package multiinstance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

const eventsWithin = 60 * time.Second

// publisherEnv is the configuration of an instance that publishes quickly and holds
// its leases briefly, so a takeover does not make the tests wait.
func publisherEnv(queue string, extra map[string]string) map[string]string {
	env := map[string]string{
		"SQS_EVENTS_QUEUE":     queue,
		"OUTBOX_POLL_INTERVAL": "100ms",
		"OUTBOX_LEASE":         "3s",
	}
	for key, value := range extra {
		env[key] = value
	}
	return env
}

type eventsHarness struct {
	db       infra.Database
	sqs      *sqs.Client
	queue    string
	queueURL string
	token    string
	owner    *pgx.Conn
}

func newEventsHarness(t *testing.T) eventsHarness {
	t.Helper()
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))
	client, err := infra.NewSQSClient(ctx, ls.Endpoint)
	require.NoError(t, err)
	name, url := infra.CreateFIFOQueue(ctx, t, client, "events")
	token, err := kc.Token(ctx, "internal-service", "internal-service-local-secret")
	require.NoError(t, err)
	owner, err := pgx.Connect(ctx, db.OwnerDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(context.Background()) })
	return eventsHarness{db: db, sqs: client, queue: name, queueURL: url, token: token, owner: owner}
}

func (h eventsHarness) start(t *testing.T, name string, extra map[string]string) *infra.Instance {
	t.Helper()
	instance := startInstance(t, name, freeAddrs(t, 1)[0], h.db, publisherEnv(h.queue, extra))
	waitReady(t, instance)
	return instance
}

// openWallet opens a funded wallet through the instance and returns its id.
func (h eventsHarness) openWallet(t *testing.T, instance *infra.Instance) string {
	t.Helper()
	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"100.00","currency":"BRL"}}`, uuid.New())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+instance.Addr+"/wallets", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(data))
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(data, &created))
	return created.ID
}

type storedEvent struct {
	ID        uuid.UUID
	Type      string
	Wallet    string
	Published bool
	Attempts  int
	Leased    bool
}

func (h eventsHarness) events(t *testing.T) []storedEvent {
	t.Helper()
	rows, err := h.owner.Query(t.Context(), `SELECT event_id, event_type, aggregate_id::text, published_at IS NOT NULL, attempts,
		locked_until IS NOT NULL AND locked_until > now() FROM outbox ORDER BY seq`)
	require.NoError(t, err)
	defer rows.Close()
	var out []storedEvent
	for rows.Next() {
		var e storedEvent
		require.NoError(t, rows.Scan(&e.ID, &e.Type, &e.Wallet, &e.Published, &e.Attempts, &e.Leased))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

func (h eventsHarness) waitAllPublished(t *testing.T, want int) []storedEvent {
	t.Helper()
	var events []storedEvent
	require.Eventually(t, func() bool {
		events = h.events(t)
		if len(events) != want {
			return false
		}
		for _, e := range events {
			if !e.Published {
				return false
			}
		}
		return true
	}, eventsWithin, 100*time.Millisecond, "all %d events to be published", want)
	return events
}

type received struct {
	infra.QueuedMessage
	EventID   string
	EventType string
}

func (h eventsHarness) receive(t *testing.T, want int) []received {
	t.Helper()
	messages, err := infra.ReceiveMessages(t.Context(), h.sqs, h.queueURL, want, eventsWithin)
	require.NoError(t, err)
	out := make([]received, len(messages))
	for i, m := range messages {
		var env struct {
			EventID   string `json:"eventId"`
			EventType string `json:"eventType"`
		}
		require.NoError(t, json.Unmarshal([]byte(m.Body), &env))
		out[i] = received{QueuedMessage: m, EventID: env.EventID, EventType: env.EventType}
	}
	return out
}

func distinct(messages []received) map[string]bool {
	out := map[string]bool{}
	for _, m := range messages {
		out[m.EventID] = true
	}
	return out
}

// The publisher dies after the queue accepted the events and before it recorded them
// as published. Nobody knows they were sent, so another instance sends them again,
// with the same event ids: nothing is lost, and the repetition is recognisable.
func TestAPublisherKilledBetweenPublishingAndMarkingLeavesTheEventsToAnotherInstance(t *testing.T) {
	h := newEventsHarness(t)
	doomed := h.start(t, "doomed", map[string]string{faultinject.EnvVar: string(faultinject.OutboxAfterPublish)})

	walletID := h.openWallet(t, doomed)

	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, doomed.WaitMessage(ctx, "fault point reached: "+string(faultinject.OutboxAfterPublish)))
	code, err := doomed.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code)

	// The queue has the first event; the database does not know.
	sent := h.events(t)
	require.Len(t, sent, 2)
	require.False(t, sent[0].Published, "it was sent, but the crash came before the mark")
	require.False(t, sent[1].Published)

	rescuer := h.start(t, "rescuer", nil)
	events := h.waitAllPublished(t, 2)
	require.Contains(t, rescuer.Logs.String(), events[0].ID.String(), "the rescuer sent the event the dead instance had already sent")

	messages := h.receive(t, 2)
	require.Equal(t, map[string]bool{events[0].ID.String(): true, events[1].ID.String(): true}, distinct(messages),
		"every event reached the queue; none was lost")
	require.Equal(t, "WagerTransactionProcessed", messages[0].EventType, "and the wallet's events are still in order")
	require.Equal(t, "WalletBalanceChanged", messages[1].EventType)
	for _, m := range messages {
		require.Equal(t, walletID, m.GroupID)
		require.Equal(t, m.EventID, m.DeduplicationID, "the repeated publication carries the same event id as its deduplication id")
	}
}

// The publisher dies holding a lease and having sent nothing.
func TestAPublisherKilledAfterReservingLeavesTheEventsToAnotherInstance(t *testing.T) {
	h := newEventsHarness(t)
	doomed := h.start(t, "doomed", map[string]string{faultinject.EnvVar: string(faultinject.OutboxAfterReserve)})
	h.openWallet(t, doomed)

	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, doomed.WaitMessage(ctx, "fault point reached: "+string(faultinject.OutboxAfterReserve)))
	code, err := doomed.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code)

	held := h.events(t)
	require.True(t, held[0].Leased, "the dead publisher still holds the event until its lease runs out")

	h.start(t, "rescuer", nil)
	events := h.waitAllPublished(t, 2)
	messages := h.receive(t, 2)
	require.Equal(t, map[string]bool{events[0].ID.String(): true, events[1].ID.String(): true}, distinct(messages))
	require.Len(t, messages, 2, "nothing had been sent before: exactly one copy each")
}

// Three instances publish at once from the same table: every event is sent, each by
// one of them, and a wallet's events arrive in order.
func TestPublishersOnSeveralInstancesNeverLoseAnEvent(t *testing.T) {
	h := newEventsHarness(t)
	instances := []*infra.Instance{h.start(t, "p1", nil), h.start(t, "p2", nil), h.start(t, "p3", nil)}

	const wallets = 30
	var wg sync.WaitGroup
	ids := make(chan string, wallets)
	for i := 0; i < wallets; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- h.openWallet(t, instances[i%len(instances)])
		}()
	}
	wg.Wait()
	close(ids)

	events := h.waitAllPublished(t, wallets*2)
	messages := h.receive(t, wallets*2)

	want := map[string]bool{}
	for _, e := range events {
		want[e.ID.String()] = true
	}
	require.Equal(t, want, distinct(messages), "every event reached the queue")
	require.Len(t, messages, wallets*2, "and none twice, since nothing failed")

	// Per wallet: the first event is the processed one, the second the balance change.
	seen := map[string][]string{}
	for _, m := range messages {
		seen[m.GroupID] = append(seen[m.GroupID], m.EventType)
	}
	require.Len(t, seen, wallets)
	for wallet, types := range seen {
		require.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, types, "wallet %s", wallet)
	}

	// Each event was sent by exactly one instance.
	for _, e := range events {
		publishers := 0
		for _, instance := range instances {
			if strings.Contains(instance.Logs.String(), e.ID.String()) {
				publishers++
			}
		}
		require.Equal(t, 1, publishers, "event %s", e.ID)
	}
}
