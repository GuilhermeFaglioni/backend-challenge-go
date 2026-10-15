// Command loadtest drives the running Compose environment with a stream of operations
// and reports what the design promises with numbers: throughput, latency percentiles,
// errors, concurrency conflicts absorbed by retries, and how far behind the outbox falls.
// It is a tool for looking at the system, not a pass/fail test: there is no target.
//
// Two shapes of load are compared with the same concurrency:
//
//   - independent: every worker has a wallet of its own, so operations never wait for each
//     other's wallet lock;
//   - hot: every worker hits the same wallet, so they are serialised by its lock.
//
// See docs/LOAD_TEST.md for the method and the recorded results.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type config struct {
	targets     []string
	keycloak    string
	scenario    string
	concurrency int
	duration    time.Duration
	warmup      time.Duration
	out         string
	label       string
	database    string
	drainWithin time.Duration
	discard     bool
}

func main() {
	var targets string
	cfg := config{}
	flag.StringVar(&targets, "targets", "http://localhost:8081,http://localhost:8082,http://localhost:8083", "base URLs of the instances, comma separated; requests are spread over them")
	flag.StringVar(&cfg.keycloak, "keycloak", "http://localhost:8180", "base URL of Keycloak")
	flag.StringVar(&cfg.scenario, "scenario", "independent", "independent (a wallet per worker) or hot (one wallet for all)")
	flag.IntVar(&cfg.concurrency, "concurrency", 32, "concurrent workers")
	flag.DurationVar(&cfg.duration, "duration", 20*time.Second, "how long to measure")
	flag.DurationVar(&cfg.warmup, "warmup", 3*time.Second, "load applied before measuring, not reported")
	flag.StringVar(&cfg.out, "out", "", "also write the report as JSON to this file")
	flag.StringVar(&cfg.label, "label", "", "a name for this run in the report")
	flag.DurationVar(&cfg.drainWithin, "drain-timeout", 3*time.Minute, "how long to wait for the outbox to catch up, before a run and after it")
	flag.BoolVar(&cfg.discard, "discard-backlog", false, "when the outbox has not caught up in -drain-timeout, mark what is left as published, so the next run starts clean (test data only: those events are never sent)")
	flag.StringVar(&cfg.database, "database", "", "connection string of the database (as its owner), to read the outbox backlog exactly; without it the instances' own gauge is used, which is coarser")
	flag.Parse()
	cfg.targets = strings.Split(targets, ",")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	report, err := run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
	fmt.Print(report.markdown())
	if cfg.out != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(cfg.out, data, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "loadtest:", err)
			os.Exit(1)
		}
	}
	if report.Inconsistent > 0 {
		os.Exit(2)
	}
}

// --- the run ---------------------------------------------------------------------------------

type report struct {
	Label         string         `json:"label,omitempty"`
	Scenario      string         `json:"scenario"`
	Instances     int            `json:"instances"`
	Concurrency   int            `json:"concurrency"`
	Wallets       int            `json:"wallets"`
	DurationSec   float64        `json:"durationSeconds"`
	Requests      int            `json:"requests"`
	Throughput    float64        `json:"requestsPerSecond"`
	P50ms         float64        `json:"p50ms"`
	P95ms         float64        `json:"p95ms"`
	P99ms         float64        `json:"p99ms"`
	MaxMs         float64        `json:"maxMs"`
	Statuses      map[string]int `json:"statuses"`
	Errors        int            `json:"errors"`
	Conflicts     map[string]int `json:"concurrencyConflictsAbsorbed"`
	OutboxLagMax  float64        `json:"outboxLagMaxSeconds"`
	OutboxBacklog int            `json:"outboxBacklogMaxEvents"`
	OutboxEvents  int            `json:"outboxEventsWritten"`
	OutboxDrainMs float64        `json:"outboxDrainAfterLoadMs"`
	OutboxLeft    int            `json:"outboxEventsLeftWhenWaitingEnded"`
	Inconsistent  int            `json:"walletsWhoseBooksDisagree"`
	Checked       int            `json:"walletsChecked"`
}

func run(ctx context.Context, cfg config) (report, error) {
	if cfg.scenario != "independent" && cfg.scenario != "hot" {
		return report{}, fmt.Errorf("unknown scenario %q", cfg.scenario)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		MaxIdleConns: 4 * cfg.concurrency, MaxIdleConnsPerHost: 2 * cfg.concurrency, IdleConnTimeout: 30 * time.Second,
	}}
	internal, err := token(ctx, client, cfg.keycloak, "internal-service", "internal-service-local-secret")
	if err != nil {
		return report{}, err
	}
	provider, err := token(ctx, client, cfg.keycloak, "provider-a", "provider-a-local-secret")
	if err != nil {
		return report{}, err
	}

	wallets := cfg.concurrency
	if cfg.scenario == "hot" {
		wallets = 1
	}
	opened, err := openWallets(ctx, client, cfg.targets[0], internal, wallets)
	if err != nil {
		return report{}, err
	}

	var outbox *outboxView
	if cfg.database != "" {
		outbox, err = openOutbox(ctx, cfg.database)
		if err != nil {
			return report{}, err
		}
		defer outbox.close()
		// Each run starts with an empty outbox, so the backlog it shows is its own.
		if !outbox.waitDrained(ctx, cfg.drainWithin) {
			if !cfg.discard {
				return report{}, errors.New("the outbox of an earlier run has not drained in time (see -drain-timeout, -discard-backlog)")
			}
			outbox.discard(ctx)
		}
	}

	before, err := scrapeAll(ctx, client, cfg.targets)
	if err != nil {
		return report{}, err
	}

	runID := uuid.NewString()[:8]
	var seq atomic.Int64
	apply := func(ctx context.Context, worker int, record func(d time.Duration, status int, err error)) {
		w := opened[worker%len(opened)]
		for ctx.Err() == nil {
			n := seq.Add(1)
			kind, amount := operationFor(n)
			target := cfg.targets[int(n)%len(cfg.targets)]
			started := time.Now()
			status, err := post(ctx, client, target, provider, runID, n, w, kind, amount)
			if ctx.Err() != nil {
				return
			}
			record(time.Since(started), status, err)
		}
	}

	// Warm up: connections, plans, caches. Not reported.
	warmCtx, cancelWarm := context.WithTimeout(ctx, cfg.warmup)
	var wg sync.WaitGroup
	for worker := range cfg.concurrency {
		wg.Add(1)
		go func() { defer wg.Done(); apply(warmCtx, worker, func(time.Duration, int, error) {}) }()
	}
	wg.Wait()
	cancelWarm()

	// Measure, sampling the outbox lag of every instance once a second.
	runCtx, cancel := context.WithTimeout(ctx, cfg.duration)
	defer cancel()
	var (
		mu        sync.Mutex
		durations []time.Duration
		statuses  = map[string]int{}
		errCount  int
	)
	var lagMax atomic.Uint64 // float64 bits
	var backlogMax atomic.Int64
	eventsBefore := 0
	if outbox != nil {
		eventsBefore = outbox.written(ctx)
		go outbox.sample(runCtx, &lagMax, &backlogMax)
	} else {
		go sampleLag(runCtx, client, cfg.targets, &lagMax)
	}

	started := time.Now()
	for worker := range cfg.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local []time.Duration
			localStatuses := map[string]int{}
			localErrors := 0
			apply(runCtx, worker, func(d time.Duration, status int, err error) {
				local = append(local, d)
				if err != nil {
					localErrors++
					localStatuses["transport error"]++
					return
				}
				localStatuses[fmt.Sprint(status)]++
			})
			mu.Lock()
			durations = append(durations, local...)
			for k, v := range localStatuses {
				statuses[k] += v
			}
			errCount += localErrors
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)

	// How long the outbox takes to catch up once the load stops.
	drainStarted := time.Now()
	var drain bool
	eventsWritten := 0
	if outbox != nil {
		eventsWritten = outbox.written(ctx) - eventsBefore
		drain = outbox.waitDrained(ctx, cfg.drainWithin)
	} else {
		drain = waitOutboxDrained(ctx, client, cfg.targets, time.Minute)
	}
	drainMs := float64(time.Since(drainStarted).Milliseconds())
	left := 0
	if !drain {
		drainMs = -1 // it did not catch up in the time given
		if outbox != nil {
			left, _, _ = outbox.backlog(ctx)
			if cfg.discard {
				outbox.discard(ctx)
			}
		}
	}

	after, err := scrapeAll(ctx, client, cfg.targets)
	if err != nil {
		return report{}, err
	}

	inconsistent, checked := reconcileAll(ctx, client, cfg.targets[0], internal, opened)

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	r := report{
		Label: cfg.label, Scenario: cfg.scenario, Instances: len(cfg.targets), Concurrency: cfg.concurrency, Wallets: wallets,
		DurationSec: elapsed.Seconds(), Requests: len(durations), Statuses: statuses, Errors: errCount,
		Conflicts: delta(before, after, "db_transaction_retries_total"), OutboxLagMax: float64frombits(lagMax.Load()),
		OutboxBacklog: int(backlogMax.Load()), OutboxEvents: eventsWritten, OutboxDrainMs: drainMs, OutboxLeft: left, Inconsistent: inconsistent, Checked: checked,
	}
	if len(durations) > 0 {
		r.Throughput = float64(len(durations)) / elapsed.Seconds()
		r.P50ms, r.P95ms, r.P99ms = ms(percentile(durations, 0.50)), ms(percentile(durations, 0.95)), ms(percentile(durations, 0.99))
		r.MaxMs = ms(durations[len(durations)-1])
	}
	return r, nil
}

// operationFor is the mix: seven bets of 1.00, two wins of 1.00 and one loss in ten, so a
// wallet's balance falls slowly and never runs out during a run.
func operationFor(n int64) (kind, amount string) {
	switch n % 10 {
	case 7, 8:
		return "WIN", "1.00"
	case 9:
		return "LOSS", "0.00"
	}
	return "BET", "1.00"
}

type wallet struct{ id, player string }

func openWallets(ctx context.Context, client *http.Client, target, internal string, n int) ([]wallet, error) {
	out := make([]wallet, n)
	for i := range out {
		player := uuid.NewString()
		body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"1000000.00","currency":"BRL"}}`, player)
		data, status, err := do(ctx, client, http.MethodPost, target+"/wallets", internal, body, nil)
		if err != nil {
			return nil, fmt.Errorf("open a wallet: %w", err)
		}
		if status != http.StatusCreated {
			return nil, fmt.Errorf("open a wallet: status %d: %s", status, data)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &created); err != nil {
			return nil, err
		}
		out[i] = wallet{id: created.ID, player: player}
	}
	return out, nil
}

func post(ctx context.Context, client *http.Client, target, token, runID string, n int64, w wallet, kind, amount string) (int, error) {
	external := fmt.Sprintf("load-%s-%d", runID, n)
	body := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,"walletId":%q,"roundId":"load-round","gameId":"load-game","kind":%q,"money":{"amount":%q,"currency":"BRL"}}`,
		external, w.player, w.id, kind, amount)
	_, status, err := do(ctx, client, http.MethodPost, target+"/wagering/transactions", token, body, map[string]string{"Idempotency-Key": "provider-a:" + external})
	return status, err
}

func reconcileAll(ctx context.Context, client *http.Client, target, internal string, wallets []wallet) (inconsistent, checked int) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, w := range wallets {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			data, status, err := do(ctx, client, http.MethodPost, target+"/wallets/"+w.id+"/reconciliation?full=true", internal, "", nil)
			var result struct {
				Consistent bool `json:"consistent"`
			}
			ok := err == nil && status == http.StatusOK && json.Unmarshal(data, &result) == nil && result.Consistent
			mu.Lock()
			defer mu.Unlock()
			checked++
			if !ok {
				inconsistent++
			}
		}()
	}
	wg.Wait()
	return inconsistent, checked
}

// --- HTTP ---------------------------------------------------------------------------------------

func do(ctx context.Context, client *http.Client, method, url, token, body string, headers map[string]string) ([]byte, int, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}

func token(ctx context.Context, client *http.Client, keycloak, clientID, secret string) (string, error) {
	form := "grant_type=client_credentials&client_id=" + clientID + "&client_secret=" + secret
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keycloak+"/realms/wagering/protocol/openid-connect/token", strings.NewReader(form))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get a token for %s: %w", clientID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil || parsed.AccessToken == "" {
		return "", fmt.Errorf("get a token for %s: status %d", clientID, resp.StatusCode)
	}
	return parsed.AccessToken, nil
}

// --- metrics ------------------------------------------------------------------------------------

// samples are the series of one scrape: "name{labels}" -> value.
type samples map[string]float64

func scrape(ctx context.Context, client *http.Client, target string) (samples, error) {
	data, status, err := do(ctx, client, http.MethodGet, target+"/metrics", "", "", nil)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", target, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: status %d", target, status)
	}
	out := samples{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		text := strings.TrimSpace(string(line))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		i := strings.LastIndex(text, " ")
		if i < 0 {
			continue
		}
		var v float64
		if _, err := fmt.Sscanf(text[i+1:], "%g", &v); err == nil {
			out[text[:i]] = v
		}
	}
	return out, nil
}

func scrapeAll(ctx context.Context, client *http.Client, targets []string) ([]samples, error) {
	var all []samples
	for _, t := range targets {
		s, err := scrape(ctx, client, t)
		if err != nil {
			return nil, err
		}
		all = append(all, s)
	}
	return all, nil
}

// delta sums, over every instance, how much each labelled series of the metric grew. The
// label is the part inside the braces, for example reason="deadlock".
func delta(before, after []samples, metric string) map[string]int {
	out := map[string]int{}
	for i := range after {
		for series, v := range after[i] {
			if !strings.HasPrefix(series, metric+"{") {
				continue
			}
			label := strings.TrimSuffix(strings.TrimPrefix(series, metric+"{"), "}")
			out[label] += int(v - before[i][series])
		}
	}
	return out
}

func sampleLag(ctx context.Context, client *http.Client, targets []string, max *atomic.Uint64) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, t := range targets {
			s, err := scrape(ctx, client, t)
			if err != nil {
				continue
			}
			if v := s["outbox_lag_seconds"]; v > float64frombits(max.Load()) {
				max.Store(float64bits(v))
			}
		}
	}
}

// waitOutboxDrained waits until no event is waiting to be published, as the instances see it.
func waitOutboxDrained(ctx context.Context, client *http.Client, targets []string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		drained := true
		for _, t := range targets {
			s, err := scrape(ctx, client, t)
			if err != nil || s["outbox_lag_seconds"] > 0 {
				drained = false
			}
		}
		if drained {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// --- numbers ------------------------------------------------------------------------------------

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

// outboxView reads the outbox table directly, as its owner: the exact backlog, which the
// instances' gauge (refreshed every two seconds by each publisher) cannot give.
type outboxView struct {
	conn *pgx.Conn
	dsn  string
}

func openOutbox(ctx context.Context, dsn string) (*outboxView, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	return &outboxView{conn: conn, dsn: dsn}, nil
}

func (o *outboxView) close() { _ = o.conn.Close(context.Background()) }

func (o *outboxView) backlog(ctx context.Context) (count int, age float64, err error) {
	return backlogOn(ctx, o.conn)
}

func backlogOn(ctx context.Context, conn *pgx.Conn) (count int, age float64, err error) {
	err = conn.QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM now() - min(occurred_at)), 0)::float8 FROM outbox WHERE published_at IS NULL`).Scan(&count, &age)
	return count, age, err
}

func (o *outboxView) written(ctx context.Context) int {
	var n int
	_ = o.conn.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n)
	return n
}

// discard marks the unpublished events as published. Only for a throwaway environment.
func (o *outboxView) discard(ctx context.Context) {
	_, _ = o.conn.Exec(ctx, `UPDATE outbox SET published_at = now(), locked_until = NULL WHERE published_at IS NULL`)
}

func (o *outboxView) waitDrained(ctx context.Context, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if n, _, err := o.backlog(ctx); err == nil && n == 0 {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// sample records the highest backlog and the highest age of the oldest unpublished event.
func (o *outboxView) sample(ctx context.Context, lagMax *atomic.Uint64, backlogMax *atomic.Int64) {
	// A connection of its own: a query cut short by the end of the run would close it.
	conn, err := pgx.Connect(ctx, o.dsn)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n, age, err := backlogOn(ctx, conn)
		if err != nil {
			continue
		}
		if int64(n) > backlogMax.Load() {
			backlogMax.Store(int64(n))
		}
		if age > float64frombits(lagMax.Load()) {
			lagMax.Store(float64bits(age))
		}
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func float64bits(f float64) uint64     { return math.Float64bits(f) }
func float64frombits(b uint64) float64 { return math.Float64frombits(b) }

func (r report) markdown() string {
	var b strings.Builder
	title := r.Scenario
	if r.Label != "" {
		title = r.Label
	}
	fmt.Fprintf(&b, "### %s\n\n", title)
	fmt.Fprintf(&b, "| | |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| scenario | %s (%d workers, %d wallet(s), %d instances) |\n", r.Scenario, r.Concurrency, r.Wallets, r.Instances)
	fmt.Fprintf(&b, "| measured for | %.1f s, %d requests |\n", r.DurationSec, r.Requests)
	fmt.Fprintf(&b, "| throughput | %.0f requests/s |\n", r.Throughput)
	fmt.Fprintf(&b, "| latency p50 / p95 / p99 / max | %.1f / %.1f / %.1f / %.1f ms |\n", r.P50ms, r.P95ms, r.P99ms, r.MaxMs)
	fmt.Fprintf(&b, "| statuses | %s |\n", formatCounts(r.Statuses))
	fmt.Fprintf(&b, "| transport errors | %d |\n", r.Errors)
	fmt.Fprintf(&b, "| concurrency conflicts absorbed by retries | %s |\n", formatCounts(r.Conflicts))
	if r.OutboxEvents > 0 {
		fmt.Fprintf(&b, "| events written to the outbox | %d |\n", r.OutboxEvents)
		fmt.Fprintf(&b, "| outbox backlog, highest sampled | %d events |\n", r.OutboxBacklog)
	}
	fmt.Fprintf(&b, "| outbox lag (age of the oldest unpublished event), highest sampled | %.2f s |\n", r.OutboxLagMax)
	if r.OutboxDrainMs >= 0 {
		fmt.Fprintf(&b, "| outbox drained after the load stopped | %.0f ms |\n", r.OutboxDrainMs)
	} else {
		fmt.Fprintf(&b, "| outbox drained after the load stopped | not within the time given; %d events were still waiting |\n", r.OutboxLeft)
	}
	fmt.Fprintf(&b, "| reconciliation after the run | %d of %d wallets disagree |\n\n", r.Inconsistent, r.Checked)
	return b.String()
}

func formatCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s: %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}
