//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/goleak"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/app"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/config"
	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// logBuffer collects the application's JSON logs so tests can assert on the
// order of lifecycle events.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// String is everything logged so far.
func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// messages returns the "msg" field of every log line, in order.
func (l *logBuffer) messages(t *testing.T) []string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var msgs []string
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Msg string `json:"msg"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "log line is not JSON: %s", line)
		msgs = append(msgs, entry.Msg)
	}
	return msgs
}

// portOpen reports whether something accepts TCP connections on addr.
func portOpen(ctx context.Context, addr string) bool {
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func appConfig(t *testing.T, db infra.Database, sqsEndpoint string) config.Config {
	t.Helper()
	return config.Config{
		InstanceID:      "integration",
		HTTPAddr:        freeAddr(t),
		LogLevel:        "info",
		ShutdownTimeout: 20 * time.Second,
		Database:        config.Database{URL: db.AppDSN, MaxConns: 4},
		AWS:             config.AWS{Region: "us-east-1", EndpointURL: sqsEndpoint},
		SQS: config.SQS{
			InputQueue: "wager-transactions.fifo", EventsQueue: "wager-events.fifo", DeadLetterQueue: "wager-transactions-dlq.fifo",
			Consumer: config.Consumer{Workers: 2, BatchSize: 10, PollWait: time.Second, ProcessingTimeout: 10 * time.Second, RetryBackoffBase: time.Second, RetryBackoffMax: 4 * time.Second},
		},
		OIDC: config.OIDC{
			IssuerURL:    kc.IssuerURL(),
			DiscoveryURL: kc.IssuerURL(),
			Audience:     "wallet-api",
		},
		Outbox: config.Outbox{PollInterval: 50 * time.Millisecond, BatchSize: 10, Lease: 5 * time.Second},
	}
}

// running is a started application. Stop is idempotent and also registered as
// a cleanup, so a failing assertion cannot leave the server and the pool open.
type running struct {
	Stop func()
	Logs *logBuffer
	Pool *pgxpool.Pool
}

// startApp builds the real composition, with its logger swapped for one that
// writes to a buffer, and starts it.
func startApp(t *testing.T, cfg config.Config) running {
	t.Helper()
	logs := &logBuffer{}
	var pool *pgxpool.Pool
	application := fx.New(
		app.New(cfg),
		fx.Decorate(func() *slog.Logger { return slog.New(slog.NewJSONHandler(logs, nil)) }),
		fx.Populate(&pool),
	)
	startCtx, cancel := context.WithTimeout(t.Context(), cfg.ShutdownTimeout)
	defer cancel()
	require.NoError(t, application.Start(startCtx), "start the application against real infrastructure")

	var once sync.Once
	stop := func() {
		once.Do(func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer stopCancel()
			assert.NoError(t, application.Stop(stopCtx), "stop within the shutdown timeout")
		})
	}
	t.Cleanup(stop)
	return running{Stop: stop, Logs: logs, Pool: pool}
}

// httpClient never keeps connections alive, so a leaked goroutine can only come
// from the application, not from the test's own client.
func httpClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

type readiness struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func getReadiness(ctx context.Context, addr string) (int, readiness, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health/ready", nil)
	if err != nil {
		return 0, readiness{}, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0, readiness{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, readiness{}, err
	}
	var r readiness
	if err := json.Unmarshal(body, &r); err != nil {
		return resp.StatusCode, readiness{}, fmt.Errorf("decode %q: %w", body, err)
	}
	return resp.StatusCode, r, nil
}

func TestApplicationStartsAndStopsAgainstRealInfrastructure(t *testing.T) {
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))
	cfg := appConfig(t, db, ls.Endpoint)

	before := goleak.IgnoreCurrent()
	application := startApp(t, cfg)

	status, report, err := getReadiness(ctx, cfg.HTTPAddr)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, readiness{Status: "ok", Checks: map[string]string{"postgres": "ok", "sqs": "ok"}}, report)
	require.NoError(t, application.Pool.Ping(ctx), "the pool works while the application runs")

	application.Stop()

	// The server must have stopped taking connections, and the pool must be
	// really closed, not just logged as closed.
	require.False(t, portOpen(ctx, cfg.HTTPAddr), "the HTTP port is still open after Stop")
	require.Error(t, application.Pool.Ping(ctx), "the pool still answers after Stop")

	// Shutdown order: components stop before the resources they use are closed.
	msgs := application.Logs.messages(t)
	httpStopped := slices.Index(msgs, "http server stopped")
	poolClosed := slices.Index(msgs, "postgres pool closed")
	require.NotEqual(t, -1, httpStopped, "missing 'http server stopped' in %v", msgs)
	require.NotEqual(t, -1, poolClosed, "missing 'postgres pool closed' in %v", msgs)
	require.Less(t, httpStopped, poolClosed, "the pool was closed before the HTTP server finished: %v", msgs)
	require.Contains(t, msgs, "sqs client closed")

	// Nothing the application started may outlive Stop.
	goleak.VerifyNone(t, before)
}

func TestStartupFailsWhenPostgresIsUnreachable(t *testing.T) {
	ctx := t.Context()
	// Nothing listens on port 1.
	cfg := appConfig(t, infra.Database{AppDSN: "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"}, ls.Endpoint)

	application := fx.New(app.New(cfg), fx.Decorate(func() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }))
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := application.Start(startCtx)
	require.Error(t, err, "the application must not start without its database")
	require.ErrorContains(t, err, "ping postgres")

	// The port must not be left open by a half-started application.
	require.False(t, portOpen(ctx, cfg.HTTPAddr))
}

// Readiness must follow the real state of each dependency. This test stops its
// own containers, so it never touches the ones shared by other tests.
func TestReadinessFollowsDependencies(t *testing.T) {
	ctx := t.Context()

	var (
		ownPG *infra.Postgres
		ownLS *infra.LocalStack
		wg    sync.WaitGroup
		pgErr error
		lsErr error
	)
	// Registered before starting, so a container that fails to boot is still
	// terminated and its logs still saved.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if t.Failed() {
			dir := os.Getenv("INTEGRATION_LOG_DIR")
			if ownPG != nil {
				_ = infra.DumpLogs(cleanupCtx, dir, "readiness-postgres", ownPG.Container)
			}
			if ownLS != nil {
				_ = infra.DumpLogs(cleanupCtx, dir, "readiness-localstack", ownLS.Container)
			}
		}
		if ownPG != nil {
			_ = ownPG.Container.Terminate(cleanupCtx)
		}
		if ownLS != nil {
			_ = ownLS.Container.Terminate(cleanupCtx)
		}
	})
	wg.Add(2)
	go func() { defer wg.Done(); ownPG, pgErr = infra.StartPostgres(ctx) }()
	go func() { defer wg.Done(); ownLS, lsErr = infra.StartLocalStack(ctx) }()
	wg.Wait()
	require.NoError(t, pgErr)
	require.NoError(t, lsErr)

	db := ownPG.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))
	cfg := appConfig(t, db, ownLS.Endpoint)
	application := startApp(t, cfg)

	readyWith := func(t *testing.T, wantStatus int, wantChecks map[string]string) {
		t.Helper()
		var lastStatus int
		var lastReport readiness
		ok := assert.Eventually(t, func() bool {
			status, report, err := getReadiness(ctx, cfg.HTTPAddr)
			lastStatus, lastReport = status, report
			return err == nil && status == wantStatus && equalChecks(report.Checks, wantChecks)
		}, 30*time.Second, 500*time.Millisecond)
		if !ok {
			t.Fatalf("readiness never became %d %v; last saw %d %+v", wantStatus, wantChecks, lastStatus, lastReport)
		}
	}

	readyWith(t, http.StatusOK, map[string]string{"postgres": "ok", "sqs": "ok"})

	// One dependency at a time, so each failure is seen on its own.
	stopTimeout := 10 * time.Second
	require.NoError(t, ownLS.Container.Stop(ctx, &stopTimeout))
	readyWith(t, http.StatusServiceUnavailable, map[string]string{"postgres": "ok", "sqs": "unavailable"})

	require.NoError(t, ownPG.Container.Stop(ctx, &stopTimeout))
	readyWith(t, http.StatusServiceUnavailable, map[string]string{"postgres": "unavailable", "sqs": "unavailable"})

	// Liveness is about the process only: it stays up while dependencies are down.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cfg.HTTPAddr+"/health/live", nil)
	require.NoError(t, err)
	resp, err := httpClient().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	application.Stop()
}

func equalChecks(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
