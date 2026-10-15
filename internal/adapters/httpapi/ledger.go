package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

const (
	defaultLedgerLimit = 50
	maxLedgerLimit     = 200
	maxCursorLength    = 200
	cursorVersion      = 1
)

var limitPattern = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)

// Ledger serves the listing of a wallet's ledger, for the internal service.
type Ledger struct {
	ledger  application.LedgerReader
	wallets application.WalletReader
	log     *slog.Logger
}

func NewLedger(ledger application.LedgerReader, wallets application.WalletReader, log *slog.Logger) *Ledger {
	return &Ledger{ledger: ledger, wallets: wallets, log: log}
}

func (h *Ledger) register(r *Routes) {
	r.Protected("GET /wallets/{walletId}/ledger", http.HandlerFunc(h.list), auth.RoleInternal)
}

// ledgerEntryView and ledgerPage are the shapes of the contract.
type ledgerEntryView struct {
	ID            uuid.UUID   `json:"id"`
	WalletID      uuid.UUID   `json:"walletId"`
	TransactionID uuid.UUID   `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     string      `json:"createdAt"`
}

type ledgerPage struct {
	Entries    []ledgerEntryView `json:"entries"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

func viewOfEntry(e wallet.LedgerEntry) ledgerEntryView {
	return ledgerEntryView{
		ID: e.ID(), WalletID: e.WalletID(), TransactionID: e.TransactionID(), Direction: string(e.Direction()),
		Money: e.Amount(), BalanceBefore: e.BalanceBefore(), BalanceAfter: e.BalanceAfter(),
		CreatedAt: e.CreatedAt().UTC().Format(timeLayout),
	}
}

func (h *Ledger) list(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	correlation := correlationID(w, req)

	id, err := parseCanonicalUUID(req.PathValue("walletId"))
	if err != nil {
		writeInvalid(w, []fieldIssue{{Field: "walletId", Issue: err.Error()}})
		return
	}
	query := req.URL.Query()
	limit, issue := parseLimit(query["limit"])
	if issue != nil {
		writeInvalid(w, []fieldIssue{*issue})
		return
	}
	var after int64
	if values := query["cursor"]; values != nil {
		var cursorIssue *fieldIssue
		if after, cursorIssue = parseCursor(values, id); cursorIssue != nil {
			writeInvalid(w, []fieldIssue{*cursorIssue})
			return
		}
	}

	if _, err := h.wallets.ByID(ctx, id); err != nil {
		h.fail(w, req, correlation, err)
		return
	}
	// One more than the page, to know whether there is a next one.
	records, err := h.ledger.List(ctx, id, after, limit+1)
	if err != nil {
		h.fail(w, req, correlation, err)
		return
	}

	page := ledgerPage{Entries: make([]ledgerEntryView, 0, min(len(records), limit))}
	for i, record := range records {
		if i == limit {
			page.NextCursor = encodeCursor(id, records[limit-1].Seq)
			break
		}
		page.Entries = append(page.Entries, viewOfEntry(record.Entry))
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Ledger) fail(w http.ResponseWriter, req *http.Request, correlation string, err error) {
	switch {
	case errors.Is(err, application.ErrWalletNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "there is no wallet with that id")
	case errors.Is(err, application.ErrTemporarilyUnavailable):
		h.log.WarnContext(req.Context(), "dependency unavailable",
			slog.String("correlationId", correlation), slog.Any("error", err))
		w.Header().Set("Retry-After", retryAfterTransient)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "a dependency is temporarily unavailable, try again shortly")
	default:
		h.log.ErrorContext(req.Context(), "request failed",
			slog.String("correlationId", correlation), slog.String("path", req.URL.Path), slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, CodeInternal, "an unexpected error occurred; quote the correlation id when reporting it")
	}
}

// parseLimit reads the page size: absent means the default; present, a plain positive
// integer up to the maximum, given once.
func parseLimit(values []string) (int, *fieldIssue) {
	if values == nil {
		return defaultLedgerLimit, nil
	}
	bad := func(text string) (int, *fieldIssue) { return 0, &fieldIssue{Field: "limit", Issue: text} }
	if len(values) != 1 {
		return bad("must be given once")
	}
	if !limitPattern.MatchString(values[0]) {
		return bad("must be a whole number from 1 to 200")
	}
	limit, _ := strconv.Atoi(values[0])
	if limit > maxLedgerLimit {
		return bad("must be a whole number from 1 to 200")
	}
	return limit, nil
}

func parseCursor(values []string, walletID uuid.UUID) (int64, *fieldIssue) {
	if len(values) != 1 {
		return 0, &fieldIssue{Field: "cursor", Issue: "must be given once"}
	}
	seq, err := decodeCursor(values[0], walletID)
	if err != nil {
		return 0, &fieldIssue{Field: "cursor", Issue: "is not a cursor of this wallet's ledger; use the nextCursor of a previous page"}
	}
	return seq, nil
}

// --- the cursor ------------------------------------------------------------------

var errBadCursor = errors.New("not a valid cursor")

// cursor says where a page ended: after which position of which wallet's ledger. It is
// encoded so that a client treats it as a token and does not build or edit one, but it
// is not a secret: the endpoint is for the internal service, and a cursor only picks a
// position in a ledger the caller may read anyway.
type cursor struct {
	Version int    `json:"v"`
	Wallet  string `json:"w"`
	Seq     int64  `json:"s"`
}

func encodeCursor(walletID uuid.UUID, seq int64) string {
	raw, err := json.Marshal(cursor{Version: cursorVersion, Wallet: walletID.String(), Seq: seq})
	if err != nil {
		panic("httpapi: a cursor of plain fields cannot fail to encode: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(text string, walletID uuid.UUID) (int64, error) {
	if text == "" || len(text) > maxCursorLength {
		return 0, errBadCursor
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil {
		return 0, errBadCursor
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c cursor
	if err := dec.Decode(&c); err != nil || dec.More() {
		return 0, errBadCursor
	}
	if dec.InputOffset() != int64(len(raw)) || c.Version != cursorVersion || c.Seq < 1 || c.Wallet != walletID.String() {
		return 0, errBadCursor
	}
	return c.Seq, nil
}
