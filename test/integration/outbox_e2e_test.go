//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

const eventsWithin = 20 * time.Second

type publishingApp struct {
	cfg       config.Config
	base      string
	token     string
	pool      *pgxpool.Pool
	sqs       *sqs.Client
	queueName string
	queueURL  string
	client    *http.Client
}

// startPublishingApp runs the whole application with its publisher against a events
// queue of the test's own. When createQueue is false the queue is named but not
// created, so the events queue is "down" until the test creates it.
func startPublishingApp(t *testing.T, createQueue bool) publishingApp {
	t.Helper()
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))
	client, err := infra.NewSQSClient(ctx, ls.Endpoint)
	require.NoError(t, err)

	app := publishingApp{sqs: client, client: httpClient()}
	if createQueue {
		app.queueName, app.queueURL = infra.CreateFIFOQueue(ctx, t, client, "events")
	} else {
		app.queueName = "events-" + uuid.NewString()[:8] + ".fifo"
	}
	app.cfg = appConfig(t, db, ls.Endpoint)
	app.cfg.SQS.EventsQueue = app.queueName
	running := startApp(t, app.cfg)
	app.pool = running.Pool
	app.base = "http://" + app.cfg.HTTPAddr
	app.token, err = kc.Token(ctx, "internal-service", "internal-service-local-secret")
	require.NoError(t, err)
	return app
}

// createQueueNow creates the queue a test left uncreated.
func (a *publishingApp) createQueueNow(t *testing.T) {
	t.Helper()
	_, err := a.sqs.CreateQueue(t.Context(), &sqs.CreateQueueInput{
		QueueName:  aws.String(a.queueName),
		Attributes: map[string]string{"FifoQueue": "true", "ContentBasedDeduplication": "false"},
	})
	require.NoError(t, err)
	out, err := a.sqs.GetQueueUrl(t.Context(), &sqs.GetQueueUrlInput{QueueName: aws.String(a.queueName)})
	require.NoError(t, err)
	a.queueURL = aws.ToString(out.QueueUrl)
	t.Cleanup(func() {
		_, _ = a.sqs.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: aws.String(a.queueURL)})
	})
}

func (a publishingApp) openWallet(t *testing.T, player uuid.UUID, amount string) (id string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.base+"/wallets",
		strings.NewReader(openBody(player, amount, "BRL")))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-Id", "corr-e2e")
	resp, err := a.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	var created walletJSON
	require.NoError(t, json.Unmarshal(body, &created))
	return created.ID
}

type outboxRow struct {
	EventID     uuid.UUID
	EventType   string
	AggregateID uuid.UUID
	Payload     string
	Published   bool
	Attempts    int
	LastError   string
}

func (a publishingApp) outbox(t *testing.T) []outboxRow {
	t.Helper()
	rows, err := a.pool.Query(t.Context(), `SELECT event_id, event_type, aggregate_id, payload::text, published_at IS NOT NULL, attempts, coalesce(last_error, '')
		FROM outbox ORDER BY seq`)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		require.NoError(t, rows.Scan(&r.EventID, &r.EventType, &r.AggregateID, &r.Payload, &r.Published, &r.Attempts, &r.LastError))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func (a publishingApp) waitAllPublished(t *testing.T, want int) []outboxRow {
	t.Helper()
	var rows []outboxRow
	require.Eventually(t, func() bool {
		rows = a.outbox(t)
		if len(rows) != want {
			return false
		}
		for _, r := range rows {
			if !r.Published {
				return false
			}
		}
		return true
	}, eventsWithin, 50*time.Millisecond, "the %d outbox events to be published", want)
	return rows
}

type envelope struct {
	EventID       string          `json:"eventId"`
	EventType     string          `json:"eventType"`
	AggregateID   string          `json:"aggregateId"`
	CorrelationID string          `json:"correlationId"`
	CausationID   string          `json:"causationId"`
	OccurredAt    string          `json:"occurredAt"`
	Version       int             `json:"version"`
	Data          json.RawMessage `json:"data"`
}

func TestAnOpenedWalletsEventsReachTheQueueInOrderAsTheStoredSnapshots(t *testing.T) {
	app := startPublishingApp(t, true)
	player := uuid.New()
	walletID := app.openWallet(t, player, "100.00")

	messages, err := infra.ReceiveMessages(t.Context(), app.sqs, app.queueURL, 2, eventsWithin)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	rows := app.waitAllPublished(t, 2)

	var types []string
	for i, message := range messages {
		var env envelope
		require.NoError(t, json.Unmarshal([]byte(message.Body), &env))
		types = append(types, env.EventType)

		// Routing: one message group per wallet, deduplicated by event id, with the
		// type as an attribute so consumers can filter without opening the body.
		require.Equal(t, walletID, message.GroupID)
		require.Equal(t, env.EventID, message.DeduplicationID)
		require.Equal(t, env.EventType, message.Attributes["eventType"])
		require.Equal(t, "corr-e2e", message.Attributes["correlationId"])

		require.Equal(t, rows[i].EventID.String(), env.EventID, "published in the order they were written")
		require.Equal(t, walletID, env.AggregateID)
		require.Equal(t, "corr-e2e", env.CorrelationID)
		require.Equal(t, 1, env.Version)
		_, err := time.Parse("2006-01-02T15:04:05.000Z07:00", env.OccurredAt)
		require.NoError(t, err, "RFC 3339 with milliseconds")
		// What was published is what was stored.
		require.JSONEq(t, rows[i].Payload, string(env.Data))
	}
	require.Equal(t, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}, types)

	t.Run("the metrics say so", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, app.base+"/metrics", nil)
		require.NoError(t, err)
		resp, err := app.client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "outbox_published_total 2")
		require.Contains(t, string(body), "outbox_publish_failures_total 0")
		require.Contains(t, string(body), "outbox_lag_seconds")
	})
}

func TestAnEmptyWalletPublishesNothing(t *testing.T) {
	app := startPublishingApp(t, true)
	app.openWallet(t, uuid.New(), "0.00")
	require.Empty(t, app.outbox(t))
	messages, err := infra.ReceiveMessages(t.Context(), app.sqs, app.queueURL, 1, 1500*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, messages)
}

// An event that was never committed does not exist for the publisher: the rows of a
// rolled-back transaction are not visible to it, so nothing is ever sent for them.
func TestARolledBackTransactionNeverProducesAPublishedEvent(t *testing.T) {
	app := startPublishingApp(t, true)

	failing := openwallet.New(openwallet.Deps{
		UnitOfWork: failingUnitOfWork{inner: postgresUnitOfWork(app.pool), failAt: "second event", err: context.DeadlineExceeded},
		Recorder:   &countingRecorder{},
		Now:        time.Now,
		NewID:      uuid.New,
	})
	_, err := failing.Execute(t.Context(), openwallet.Input{PlayerID: uuid.New(), InitialBalance: brl(t, 5000), CorrelationID: "c"})
	require.Error(t, err)

	require.Empty(t, app.outbox(t), "the rolled-back events are gone")
	messages, err := infra.ReceiveMessages(t.Context(), app.sqs, app.queueURL, 1, 1500*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, messages, "and nothing was sent for them")
}

func TestWhileTheQueueIsUnavailableEventsStayAndResumeWhenItReturns(t *testing.T) {
	app := startPublishingApp(t, false)
	walletID := app.openWallet(t, uuid.New(), "100.00")

	// The events are committed and the queue is not there: they are retried with a
	// growing delay, and stay unpublished.
	require.Eventually(t, func() bool {
		rows := app.outbox(t)
		return len(rows) == 2 && rows[0].Attempts >= 1
	}, eventsWithin, 50*time.Millisecond, "the first event to have been tried and to have failed")
	rows := app.outbox(t)
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.False(t, r.Published)
	}
	require.NotEmpty(t, rows[0].LastError, "the reason is recorded")
	require.Zero(t, rows[1].Attempts, "the second waits behind the first: it is not even tried")

	// The queue comes back; the events follow without anyone touching them.
	app.createQueueNow(t)
	app.waitAllPublished(t, 2)
	messages, err := infra.ReceiveMessages(t.Context(), app.sqs, app.queueURL, 2, eventsWithin)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	var first envelope
	require.NoError(t, json.Unmarshal([]byte(messages[0].Body), &first))
	require.Equal(t, "WagerTransactionProcessed", first.EventType, "and still in order")
	require.Equal(t, walletID, first.AggregateID)
}

func postgresUnitOfWork(pool *pgxpool.Pool) application.UnitOfWork {
	return newUnitOfWork(pool)
}
