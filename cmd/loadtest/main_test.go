package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPercentilesOfASortedList(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	for p, want := range map[float64]time.Duration{0.50: 50, 0.95: 95, 0.99: 99, 1.0: 100, 0.0: 1} {
		if got := percentile(d, p); got != want*time.Millisecond {
			t.Errorf("p%v = %v, want %vms", p*100, got, want)
		}
	}
	if percentile(nil, 0.5) != 0 {
		t.Error("an empty list has no percentile")
	}
	if got := percentile([]time.Duration{7 * time.Millisecond}, 0.99); got != 7*time.Millisecond {
		t.Errorf("one sample is every percentile, got %v", got)
	}
}

func TestTheMixIsSevenBetsTwoWinsAndALossInTen(t *testing.T) {
	counts := map[string]int{}
	for n := int64(1); n <= 1000; n++ {
		kind, amount := operationFor(n)
		counts[kind]++
		if (kind == "LOSS") != (amount == "0.00") {
			t.Fatalf("%s with %s", kind, amount)
		}
	}
	if counts["BET"] != 700 || counts["WIN"] != 200 || counts["LOSS"] != 100 {
		t.Fatalf("got %v", counts)
	}
}

func TestMetricsAreScrapedAndTheGrowthOfALabelledSeriesIsSummedOverInstances(t *testing.T) {
	body := func(deadlocks, serialization string) string {
		return "# HELP db_transaction_retries_total x\n# TYPE db_transaction_retries_total counter\n" +
			"db_transaction_retries_total{reason=\"deadlock\"} " + deadlocks + "\n" +
			"db_transaction_retries_total{reason=\"serialization_failure\"} " + serialization + "\n" +
			"outbox_lag_seconds 1.5\n\n"
	}
	respond := func(text *string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer scraper-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(*text))
		}))
	}
	a, b := body("1", "0"), body("2", "5")
	srvA, srvB := respond(&a), respond(&b)
	defer srvA.Close()
	defer srvB.Close()
	targets := []string{srvA.URL, srvB.URL}

	before, err := scrapeAll(context.Background(), http.DefaultClient, targets, "scraper-token")
	if err != nil {
		t.Fatal(err)
	}
	if before[0]["outbox_lag_seconds"] != 1.5 {
		t.Errorf("a series without labels was not read: %v", before[0])
	}
	a, b = body("4", "0"), body("2", "9")
	after, err := scrapeAll(context.Background(), http.DefaultClient, targets, "scraper-token")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := scrapeAll(context.Background(), http.DefaultClient, targets, ""); err == nil {
		t.Error("the metrics are protected: a scrape without the token must fail")
	}

	got := delta(before, after, "db_transaction_retries_total")
	if got[`reason="deadlock"`] != 3 || got[`reason="serialization_failure"`] != 4 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestAnInstanceThatDoesNotAnswerMetricsIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	if _, err := scrape(context.Background(), http.DefaultClient, srv.URL, "scraper-token"); err == nil {
		t.Fatal("a failing scrape must not look like zero")
	}
}

func TestTheReportShowsWhatWasMeasuredAndAnUndrainedOutbox(t *testing.T) {
	r := report{
		Label: "A run", Scenario: "hot", Instances: 3, Concurrency: 16, Wallets: 1, DurationSec: 20, Requests: 1000, Throughput: 50,
		P50ms: 12.5, P95ms: 40, P99ms: 90, MaxMs: 120, Statuses: map[string]int{"200": 990, "503": 10}, Errors: 0,
		Conflicts: map[string]int{}, OutboxEvents: 2000, OutboxBacklog: 1500, OutboxLagMax: 3.25, OutboxDrainMs: -1, OutboxLeft: 700,
		Checked: 1,
	}
	text := r.markdown()
	for _, want := range []string{
		"### A run", "hot (16 workers, 1 wallet(s), 3 instances)", "50 requests/s", "12.5 / 40.0 / 90.0 / 120.0 ms",
		"200: 990, 503: 10", "none", "2000", "1500 events", "3.25 s", "700 events were still waiting", "0 of 1 wallets disagree",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the report does not show %q:\n%s", want, text)
		}
	}
	r.OutboxDrainMs = 1234
	if !strings.Contains(r.markdown(), "1234 ms") {
		t.Error("a drain time is shown in milliseconds")
	}
}

func TestAnUnknownScenarioIsRefusedBeforeAnythingIsSent(t *testing.T) {
	if _, err := run(context.Background(), config{scenario: "sideways"}); err == nil {
		t.Fatal("expected an error")
	}
}
