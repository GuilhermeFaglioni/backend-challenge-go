//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
)

type ledgerEntryJSON struct {
	ID            string `json:"id"`
	TransactionID string `json:"transactionId"`
	Direction     string `json:"direction"`
	Money         struct{ Amount string }
	BalanceBefore struct{ Amount string } `json:"balanceBefore"`
	BalanceAfter  struct{ Amount string } `json:"balanceAfter"`
}

type ledgerPageJSON struct {
	Entries    []ledgerEntryJSON     `json:"entries"`
	NextCursor string                `json:"nextCursor"`
	Error      struct{ Code string } `json:"error"`
}

func (a walletAPI) ledger(t *testing.T, client *contractClient, token, walletID, query string) (int, ledgerPageJSON) {
	t.Helper()
	path := "/wallets/" + walletID + "/ledger"
	if query != "" {
		path += "?" + query
	}
	status, _, raw := client.call(t.Context(), http.MethodGet, a.base+path, token, "")
	var page ledgerPageJSON
	_ = json.Unmarshal([]byte(raw), &page)
	return status, page
}

// walk follows the cursors from the beginning to the end and returns every entry seen.
func (a walletAPI) walk(t *testing.T, client *contractClient, walletID string, limit int) []ledgerEntryJSON {
	t.Helper()
	var all []ledgerEntryJSON
	query := fmt.Sprintf("limit=%d", limit)
	for pages := 0; ; pages++ {
		status, page := a.ledger(t, client, a.internal, walletID, query)
		require.Equal(t, http.StatusOK, status)
		all = append(all, page.Entries...)
		if page.NextCursor == "" {
			return all
		}
		require.Less(t, pages, 100_000, "the walk does not end")
		query = fmt.Sprintf("limit=%d&cursor=%s", limit, page.NextCursor)
	}
}

// requireChain checks that the entries, in the order given, are the whole history of a
// wallet from its opening, or a prefix of it: the first starts from zero and each one
// starts where the previous one ended. An entry skipped or repeated breaks the chain.
func requireChain(t *testing.T, entries []ledgerEntryJSON) {
	t.Helper()
	ids := map[string]bool{}
	for i, e := range entries {
		require.False(t, ids[e.ID], "entry %s appears twice", e.ID)
		ids[e.ID] = true
		if i == 0 {
			require.Equal(t, "0.00", e.BalanceBefore.Amount, "the ledger starts with the opening")
			continue
		}
		require.Equal(t, entries[i-1].BalanceAfter.Amount, e.BalanceBefore.Amount, "entry %d does not start where entry %d ended", i, i-1)
	}
}

func TestTheLedgerIsListedPageByPageIncludingTheOpening(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	for i := 1; i <= 7; i++ {
		status, _, _ := a.submit(t, a.strict, a.provider, fmt.Sprintf("k-%d", i), operation(w, fmt.Sprintf("tx-%d", i), "BET", "1.00"))
		require.Equal(t, http.StatusOK, status)
	}

	status, first := a.ledger(t, a.strict, a.internal, w.id, "limit=3")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, first.Entries, 3)
	require.NotEmpty(t, first.NextCursor)
	require.Equal(t, "CREDIT", first.Entries[0].Direction, "the opening comes first")
	require.Equal(t, "100.00", first.Entries[0].Money.Amount)
	require.Equal(t, "0.00", first.Entries[0].BalanceBefore.Amount)

	all := a.walk(t, a.strict, w.id, 3)
	require.Len(t, all, 8, "the opening and seven bets")
	requireChain(t, all)
	require.Equal(t, "93.00", all[len(all)-1].BalanceAfter.Amount)
	for _, e := range all[1:] {
		require.Equal(t, "DEBIT", e.Direction)
	}

	status, empty := a.ledger(t, a.strict, a.internal, a.openFunded(t, "0.00").id, "")
	require.Equal(t, http.StatusOK, status)
	require.NotNil(t, empty.Entries, "an empty ledger is an empty list")
	require.Empty(t, empty.Entries)
	require.Empty(t, empty.NextCursor)
	a.strict.requireNoViolations()
}

func TestTheLedgerListingRefusesWhatItShouldAndTouchesNothing(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "10.00")
	other := a.openFunded(t, "10.00")
	_, page := a.ledger(t, a.client, a.internal, w.id, "limit=1")
	// A cursor needs a second page: make one.
	for i := 1; i <= 2; i++ {
		a.submit(t, a.client, a.provider, fmt.Sprintf("k-%d", i), operation(w, fmt.Sprintf("tx-%d", i), "BET", "1.00"))
	}
	_, page = a.ledger(t, a.client, a.internal, w.id, "limit=1")
	require.NotEmpty(t, page.NextCursor)
	before := a.tables(t)

	for name, query := range map[string]string{
		"limit zero":              "limit=0",
		"limit too large":         "limit=201",
		"limit not a number":      "limit=many",
		"a cursor that is junk":   "cursor=nope",
		"another wallet's cursor": "cursor=" + func() string { _, p := a.ledger(t, a.client, a.internal, w.id, "limit=1"); return p.NextCursor }(),
	} {
		target := w.id
		if name == "another wallet's cursor" {
			target = other.id
		}
		status, body := a.ledger(t, a.client, a.internal, target, query)
		require.Equal(t, http.StatusBadRequest, status, name)
		require.Equal(t, "INVALID_REQUEST", body.Error.Code, name)
	}

	status, body := a.ledger(t, a.client, a.internal, uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "NOT_FOUND", body.Error.Code)

	status, _ = a.ledger(t, a.client, a.provider, w.id, "")
	require.Equal(t, http.StatusForbidden, status, "a provider does not read ledgers")
	status, _ = a.ledger(t, a.client, "", w.id, "")
	require.Equal(t, http.StatusUnauthorized, status)

	require.Equal(t, before, a.tables(t))
	a.client.requireNoViolations()
}

// The order is the database's sequence, not the clock: entries written with the very same
// timestamp, as two instances whose clocks agree to the millisecond can, are still listed
// in the order they were written.
func TestTheLedgerIsListedInTheOrderEntriesWereWrittenNotByTheirTimestamps(t *testing.T) {
	s := newSchema(t)
	pool := appPool(s.ctx, t, s.appDSN)
	wallet, player := s.wallet(t)

	balance := int64(0)
	var written []uuid.UUID
	for i := 0; i < 40; i++ {
		tx := s.credit(t, wallet, player, balance, 100, int64(i)+2) // every entry carries the same created_at
		written = append(written, tx)
		balance += 100
	}

	reader := postgres.NewLedgerReader(pool)
	var listed []uuid.UUID
	after := int64(0)
	for {
		records, err := reader.List(s.ctx, wallet, after, 7)
		require.NoError(t, err)
		if len(records) == 0 {
			break
		}
		for _, r := range records {
			listed = append(listed, r.Entry.TransactionID())
			require.Greater(t, r.Seq, after, "the positions only go up")
			after = r.Seq
		}
	}
	require.Equal(t, written, listed, "in the order they were written, however the timestamps tie")
}

// Reading while writing: a walk over the ledger of a wallet that is receiving bets sees
// each entry once and none skipped. The chain is the proof: an entry missed or repeated
// leaves an entry that does not start where the previous one ended.
func TestWalkingTheLedgerWhileItIsBeingWrittenSeesEveryEntryOnce(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "10000.00")

	// More entries than a page, before the walks begin.
	for i := 0; i < 60; i++ {
		status, _, _ := a.submit(t, a.client, a.provider, fmt.Sprintf("seed-%d", i), operation(w, fmt.Sprintf("seed-tx-%d", i), "BET", "1.00"))
		require.Equal(t, http.StatusOK, status)
	}

	const writers, betsPerWriter = 4, 40
	var written atomic.Int64
	var wg sync.WaitGroup
	done := make(chan struct{})
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < betsPerWriter; i++ {
				status, _, _ := a.submit(t, a.client, a.provider, fmt.Sprintf("w%d-%d", writer, i), operation(w, fmt.Sprintf("w%d-tx-%d", writer, i), "BET", "1.00"))
				if status == http.StatusOK {
					written.Add(1)
				}
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()

	walks, longest := 0, 0
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}
		entries := a.walk(t, a.client, w.id, 7)
		requireChain(t, entries)
		walks++
		longest = max(longest, len(entries))
	}
	require.Greater(t, walks, 1, "the walks must overlap the writes to prove anything")

	total := 1 + 60 + int(written.Load())
	require.Equal(t, 1+60+writers*betsPerWriter, total, "every concurrent bet was processed")
	final := a.walk(t, a.client, w.id, 50)
	require.Len(t, final, total, "the last walk sees all of it")
	requireChain(t, final)
	require.Equal(t, fmt.Sprintf("%d.00", 10000-60-writers*betsPerWriter), final[len(final)-1].BalanceAfter.Amount)
	t.Logf("%d walks overlapped the writes; the longest saw %d of %d entries", walks, longest, total)
}
