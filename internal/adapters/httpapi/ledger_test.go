package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// fakeLedger serves a fixed ledger the way the database does: in order, after a position.
type fakeLedger struct {
	records []application.LedgerRecord
	err     error
	calls   []ledgerCall
}

type ledgerCall struct {
	wallet uuid.UUID
	after  int64
	limit  int
}

func (f *fakeLedger) List(_ context.Context, id uuid.UUID, after int64, limit int) ([]application.LedgerRecord, error) {
	f.calls = append(f.calls, ledgerCall{id, after, limit})
	if f.err != nil {
		return nil, f.err
	}
	var out []application.LedgerRecord
	for _, r := range f.records {
		if r.Seq > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// ledgerOf builds n entries of a wallet, alternating... all credits of 1.00 growing the balance.
func ledgerOf(t *testing.T, walletID uuid.UUID, n int) []application.LedgerRecord {
	t.Helper()
	records := make([]application.LedgerRecord, n)
	for i := range records {
		entry, err := wallet.NewLedgerEntry(wallet.LedgerEntryParams{
			ID: uuid.New(), WalletID: walletID, TransactionID: uuid.New(), Direction: wallet.DirectionCredit,
			Amount: brl(t, 100), BalanceBefore: brl(t, int64(i)*100), BalanceAfter: brl(t, int64(i+1)*100),
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
		records[i] = application.LedgerRecord{Seq: int64(10 + i*3), Entry: entry} // gaps: a sequence has them
	}
	return records
}

type ledgerEnv struct {
	ledger   *fakeLedger
	wallets  *fakeReader
	routes   *Routes
	walletID uuid.UUID
}

func newLedgerEnv(t *testing.T, entries int) *ledgerEnv {
	t.Helper()
	id := uuid.New()
	e := &ledgerEnv{ledger: &fakeLedger{records: ledgerOf(t, id, entries)}, wallets: &fakeReader{result: fundedWallet(t, 100)}, walletID: id}
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	e.routes = NewRoutes(fakeAuthenticator{
		"internal-token": {principal: internal},
		"provider-token": {principal: providerA},
	}, logger, prometheus.NewRegistry())
	NewLedger(e.ledger, e.wallets, logger).register(e.routes)
	return e
}

func (e *ledgerEnv) get(token, query string) *httptest.ResponseRecorder {
	path := "/wallets/" + e.walletID.String() + "/ledger"
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.routes.Handler().ServeHTTP(rec, req)
	return rec
}

type pageJSON struct {
	Entries []struct {
		ID            string `json:"id"`
		WalletID      string `json:"walletId"`
		TransactionID string `json:"transactionId"`
		Direction     string `json:"direction"`
		Money         struct{ Amount, Currency string }
		BalanceBefore struct{ Amount string } `json:"balanceBefore"`
		BalanceAfter  struct{ Amount string } `json:"balanceAfter"`
		CreatedAt     string                  `json:"createdAt"`
	} `json:"entries"`
	NextCursor string `json:"nextCursor"`
}

func decodePage(t *testing.T, rec *httptest.ResponseRecorder) pageJSON {
	t.Helper()
	var page pageJSON
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page), rec.Body.String())
	return page
}

// --- a page --------------------------------------------------------------------------

func TestAPageShowsTheEntriesOldestFirst(t *testing.T) {
	e := newLedgerEnv(t, 3)
	rec := e.get("internal-token", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	page := decodePage(t, rec)
	require.Len(t, page.Entries, 3)
	require.Empty(t, page.NextCursor, "the last page has no cursor")

	first := page.Entries[0]
	require.Equal(t, e.ledger.records[0].Entry.ID().String(), first.ID)
	require.Equal(t, e.walletID.String(), first.WalletID)
	require.Equal(t, e.ledger.records[0].Entry.TransactionID().String(), first.TransactionID)
	require.Equal(t, "CREDIT", first.Direction)
	require.Equal(t, "1.00", first.Money.Amount)
	require.Equal(t, "BRL", first.Money.Currency)
	require.Equal(t, "0.00", first.BalanceBefore.Amount)
	require.Equal(t, "1.00", first.BalanceAfter.Amount)
	require.Equal(t, "2026-10-14T12:00:00.000Z", first.CreatedAt)
	require.Equal(t, "3.00", page.Entries[2].BalanceAfter.Amount)
}

func TestAnEmptyLedgerIsAnEmptyListNotNull(t *testing.T) {
	e := newLedgerEnv(t, 0)
	rec := e.get("internal-token", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"entries":[]}`, rec.Body.String(), "an array, so a client can iterate it")
}

func TestWalkingTheCursorsSeesEveryEntryOnceInOrder(t *testing.T) {
	for _, n := range []int{1, 49, 50, 51, 100, 101, 137} {
		e := newLedgerEnv(t, n)
		var seen []string
		query := ""
		pages := 0
		for {
			page := decodePage(t, e.get("internal-token", query))
			for _, entry := range page.Entries {
				seen = append(seen, entry.ID)
			}
			pages++
			if page.NextCursor == "" {
				break
			}
			query = "cursor=" + page.NextCursor
			require.LessOrEqual(t, pages, n, "the walk does not end")
		}
		require.Len(t, seen, n, "%d entries", n)
		for i, record := range e.ledger.records {
			require.Equal(t, record.Entry.ID().String(), seen[i], "entry %d of %d is out of place or repeated", i, n)
		}
		wantPages := (n + 49) / 50
		require.Equal(t, wantPages, pages, "%d entries at 50 per page", n)
	}
}

func TestAPageHasAMoreCursorExactlyWhenThereIsMore(t *testing.T) {
	e := newLedgerEnv(t, 50)
	require.Empty(t, decodePage(t, e.get("internal-token", "limit=50")).NextCursor, "exactly a page, nothing after it")
	e = newLedgerEnv(t, 51)
	require.NotEmpty(t, decodePage(t, e.get("internal-token", "limit=50")).NextCursor)
}

func TestTheLimitHasADefaultAndBounds(t *testing.T) {
	e := newLedgerEnv(t, 300)
	require.Len(t, decodePage(t, e.get("internal-token", "")).Entries, 50, "the default")
	require.Len(t, decodePage(t, e.get("internal-token", "limit=1")).Entries, 1)
	require.Len(t, decodePage(t, e.get("internal-token", "limit=200")).Entries, 200, "the maximum")
	for _, bad := range []string{"limit=0", "limit=-1", "limit=201", "limit=1000000", "limit=abc", "limit=", "limit=1.5", "limit=+5", "limit=05", "limit=1&limit=2"} {
		rec := e.get("internal-token", bad)
		require.Equal(t, http.StatusBadRequest, rec.Code, bad)
		body := decodeError(t, rec)
		require.Equal(t, CodeInvalidRequest, body.Error.Code)
		require.Equal(t, "limit", body.Error.Details[0].Field)
	}
	for _, call := range e.ledger.calls {
		require.LessOrEqual(t, call.limit, 201, "never asks the database for more than a page and one more")
	}
}

// --- the cursor ----------------------------------------------------------------------

func TestACursorRoundTripsAndIsBoundToItsWallet(t *testing.T) {
	id := uuid.New()
	for _, seq := range []int64{1, 2, 99, 1 << 40, 1<<62 - 1} {
		got, err := decodeCursor(encodeCursor(id, seq), id)
		require.NoError(t, err)
		require.Equal(t, seq, got)
	}
	_, err := decodeCursor(encodeCursor(id, 5), uuid.New())
	require.ErrorIs(t, err, errBadCursor, "a cursor of another wallet")
}

func TestACursorLooksLikeNothingInParticular(t *testing.T) {
	cursor := encodeCursor(uuid.New(), 12345)
	require.Regexp(t, `^[A-Za-z0-9_-]+$`, cursor, "URL safe, no padding")
	require.NotContains(t, cursor, "12345", "not readable as a number a client might increment")
}

func TestOnlyAWellFormedCursorIsAccepted(t *testing.T) {
	id := uuid.New()
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	cases := map[string]string{
		"not base64":            "###",
		"padded base64":         base64.URLEncoding.EncodeToString([]byte(`{"v":1,"w":"`+id.String()+`","s":5}`)) + "==",
		"not json":              enc("hello"),
		"empty json":            enc(`{}`),
		"another version":       enc(`{"v":2,"w":"` + id.String() + `","s":5}`),
		"a zero position":       enc(`{"v":1,"w":"` + id.String() + `","s":0}`),
		"a negative position":   enc(`{"v":1,"w":"` + id.String() + `","s":-5}`),
		"a position as string":  enc(`{"v":1,"w":"` + id.String() + `","s":"5"}`),
		"an unknown field":      enc(`{"v":1,"w":"` + id.String() + `","s":5,"x":1}`),
		"another wallet":        enc(`{"v":1,"w":"` + uuid.NewString() + `","s":5}`),
		"a wallet that is junk": enc(`{"v":1,"w":"nope","s":5}`),
		"trailing data":         enc(`{"v":1,"w":"` + id.String() + `","s":5}{}`),
		"too long":              strings.Repeat("a", 201),
		"empty":                 "",
	}
	for name, cursor := range cases {
		_, err := decodeCursor(cursor, id)
		require.ErrorIs(t, err, errBadCursor, name)
	}
}

// A cursor has one spelling. A base64 text whose last character carries bits that are
// ignored decodes to the same bytes as the original, but it is not the cursor that was
// issued, and a token should not be accepted in forms nobody issued.
func TestAnotherSpellingOfACursorIsNotACursor(t *testing.T) {
	id := uuid.New()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	variants := 0
	for seq := int64(1); seq <= 40 && variants == 0; seq++ {
		original := encodeCursor(id, seq)
		want, err := base64.RawURLEncoding.DecodeString(original)
		require.NoError(t, err)
		for _, c := range alphabet {
			variant := original[:len(original)-1] + string(c)
			if variant == original {
				continue
			}
			got, err := base64.RawURLEncoding.DecodeString(variant) // the lenient decoder
			if err != nil || !bytes.Equal(got, want) {
				continue
			}
			variants++
			_, err = decodeCursor(variant, id)
			require.ErrorIs(t, err, errBadCursor, "%q is %q spelled differently", variant, original)
		}
	}
	require.NotZero(t, variants, "the test never met a cursor with a second spelling, so it proved nothing")
}

// Padding made of whitespace is valid JSON and would decode, but a cursor is never that
// long: a size limit keeps anything absurd from being decoded at all.
func TestACursorBeyondTheSizeLimitIsRefusedEvenIfItWouldDecode(t *testing.T) {
	id := uuid.New()
	inner := fmt.Sprintf(`{"v":1,"w":%q,"s":5}`, id)
	short := base64.RawURLEncoding.EncodeToString([]byte(inner))
	_, err := decodeCursor(short, id)
	require.NoError(t, err)

	padded := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(" ", 300) + inner))
	require.Greater(t, len(padded), maxCursorLength)
	_, err = decodeCursor(padded, id)
	require.ErrorIs(t, err, errBadCursor)
}

func TestABadCursorIs400AndReadsNothing(t *testing.T) {
	e := newLedgerEnv(t, 10)
	for _, bad := range []string{"cursor=nope", "cursor=", "cursor=a&cursor=b", "cursor=" + encodeCursor(uuid.New(), 3)} {
		rec := e.get("internal-token", bad)
		require.Equal(t, http.StatusBadRequest, rec.Code, bad)
		require.Equal(t, "cursor", decodeError(t, rec).Error.Details[0].Field)
	}
	require.Empty(t, e.ledger.calls)
}

func TestTheCursorContinuesFromTheLastEntryOfThePage(t *testing.T) {
	e := newLedgerEnv(t, 10)
	page := decodePage(t, e.get("internal-token", "limit=4"))
	require.NotEmpty(t, page.NextCursor)
	after, err := decodeCursor(page.NextCursor, e.walletID)
	require.NoError(t, err)
	require.Equal(t, e.ledger.records[3].Seq, after, "the position of the fourth entry, which is the last one shown")
}

// --- the wallet and who may ask ------------------------------------------------------

func TestAnUnknownWalletIs404(t *testing.T) {
	e := newLedgerEnv(t, 3)
	e.wallets.err = application.ErrWalletNotFound
	rec := e.get("internal-token", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, CodeNotFound, decodeError(t, rec).Error.Code)
	require.Empty(t, e.ledger.calls)
}

func TestABadWalletIdIs400(t *testing.T) {
	e := newLedgerEnv(t, 3)
	for _, id := range []string{"nope", "3fa85f6457174562b3fc2c963f66afa6", "00000000-0000-0000-0000-000000000000"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/wallets/"+id+"/ledger", nil)
		req.Header.Set("Authorization", "Bearer internal-token")
		rec := httptest.NewRecorder()
		e.routes.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, id)
	}
}

func TestOnlyTheInternalRoleMayListALedger(t *testing.T) {
	e := newLedgerEnv(t, 3)
	require.Equal(t, http.StatusForbidden, e.get("provider-token", "").Code)
	require.Equal(t, http.StatusUnauthorized, e.get("", "").Code)
	require.Equal(t, http.StatusUnauthorized, e.get("garbage", "").Code)
	require.Empty(t, e.ledger.calls)
	require.Empty(t, e.wallets.calls)
}

func TestLedgerFailuresAreMappedWithoutLeakingInternals(t *testing.T) {
	e := newLedgerEnv(t, 3)
	e.ledger.err = fmt.Errorf("%w: password=hunter2", application.ErrTemporarilyUnavailable)
	rec := e.get("internal-token", "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.NotContains(t, rec.Body.String(), "hunter2")

	e.ledger.err = errors.New("password=hunter2")
	rec = e.get("internal-token", "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "hunter2")
}

func TestTheWalletIsReadBeforeTheLedger(t *testing.T) {
	e := newLedgerEnv(t, 3)
	e.get("internal-token", "")
	require.Equal(t, []uuid.UUID{e.walletID}, e.wallets.calls)
	require.Len(t, e.ledger.calls, 1)
}
