package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/openwallet"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// maxBodyBytes caps a request body; a wallet request is a few hundred bytes.
const maxBodyBytes = 64 << 10

// retryAfterTransient is what clients are told to wait when a dependency is
// temporarily unavailable.
const retryAfterTransient = "1"

// WalletOpener opens wallets (the openwallet use case).
type WalletOpener interface {
	Execute(ctx context.Context, in openwallet.Input) (wallet.Wallet, error)
}

// Wallets serves the wallet endpoints. Both are for the internal service.
type Wallets struct {
	opener WalletOpener
	reader application.WalletReader
	log    *slog.Logger
}

func NewWallets(opener WalletOpener, reader application.WalletReader, log *slog.Logger) *Wallets {
	return &Wallets{opener: opener, reader: reader, log: log}
}

func (h *Wallets) register(r *Routes) {
	r.Protected("POST /wallets", http.HandlerFunc(h.open), auth.RoleInternal)
	r.Protected("GET /wallets/{walletId}", http.HandlerFunc(h.get), auth.RoleInternal)
}

// walletResponse is the shape of a wallet in the contract.
type walletResponse struct {
	ID       uuid.UUID   `json:"id"`
	PlayerID uuid.UUID   `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

func toResponse(w wallet.Wallet) walletResponse {
	return walletResponse{ID: w.ID(), PlayerID: w.PlayerID(), Balance: w.Balance(), Version: w.Version()}
}

// openRequest holds the fields raw, so each can be checked and reported on its own.
type openRequest struct {
	PlayerID       json.RawMessage `json:"playerId"`
	InitialBalance json.RawMessage `json:"initialBalance"`
}

func (h *Wallets) open(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(w, r)

	var body openRequest
	if issues := decodeBody(w, r, &body); len(issues) > 0 {
		writeInvalid(w, issues)
		return
	}
	var issues []fieldIssue
	player, _ := parseUUIDField(body.PlayerID, "playerId", &issues)
	initial, _ := parseMoneyField(body.InitialBalance, "initialBalance", &issues)
	if len(issues) > 0 {
		writeInvalid(w, issues)
		return
	}

	created, err := h.opener.Execute(r.Context(), openwallet.Input{PlayerID: player, InitialBalance: initial, CorrelationID: correlation})
	if err != nil {
		h.fail(w, r, correlation, err)
		return
	}
	h.log.InfoContext(r.Context(), "wallet opened",
		slog.String("walletId", created.ID().String()),
		slog.String("correlationId", correlation),
	)
	w.Header().Set("Location", "/wallets/"+created.ID().String())
	writeJSON(w, http.StatusCreated, toResponse(created))
}

func (h *Wallets) get(w http.ResponseWriter, r *http.Request) {
	correlation := correlationID(w, r)

	id, err := parseCanonicalUUID(r.PathValue("walletId"))
	if err != nil {
		writeInvalid(w, []fieldIssue{{Field: "walletId", Issue: err.Error()}})
		return
	}
	found, err := h.reader.ByID(r.Context(), id)
	if err != nil {
		h.fail(w, r, correlation, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(found))
}

// fail answers a use case or repository error. Only what is safe to say reaches the
// client; the error itself goes to the log with the correlation id.
func (h *Wallets) fail(w http.ResponseWriter, r *http.Request, correlation string, err error) {
	var conflict *application.WalletAlreadyExistsError
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, errorBody{Error: errorDetail{
			Code:     CodeWalletAlreadyExists,
			Message:  "the player already has a wallet in that currency",
			WalletID: conflict.WalletID.String(),
		}})
	case errors.Is(err, application.ErrWalletNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "there is no wallet with that id")
	case errors.Is(err, openwallet.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, application.ErrTemporarilyUnavailable):
		h.log.WarnContext(r.Context(), "dependency unavailable",
			slog.String("correlationId", correlation), slog.Any("error", err))
		w.Header().Set("Retry-After", retryAfterTransient)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "a dependency is temporarily unavailable, try again shortly")
	default:
		h.log.ErrorContext(r.Context(), "request failed",
			slog.String("correlationId", correlation), slog.String("path", r.URL.Path), slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, CodeInternal, "an unexpected error occurred; quote the correlation id when reporting it")
	}
}

// --- request parsing ---------------------------------------------------------------

func writeInvalid(w http.ResponseWriter, issues []fieldIssue) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{
		Code: CodeInvalidRequest, Message: "the request is not valid", Details: issues,
	}})
}

// decodeBody reads exactly one JSON object, no larger than maxBodyBytes and with no
// field the contract does not name. It returns what is wrong, if anything.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) []fieldIssue {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		var tooBig *http.MaxBytesError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.Is(err, io.EOF):
			return []fieldIssue{{Field: "body", Issue: "a JSON object is required"}}
		case errors.As(err, &tooBig):
			return []fieldIssue{{Field: "body", Issue: "the request body is too large"}}
		case errors.As(err, &typeErr):
			return []fieldIssue{{Field: "body", Issue: "a JSON object is required"}}
		default:
			return []fieldIssue{{Field: "body", Issue: err.Error()}}
		}
	}
	// A second document or trailing garbage after the first is not a valid body.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return []fieldIssue{{Field: "body", Issue: "unexpected data after the JSON object"}}
	}
	return nil
}

func isNullOrAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

func parseUUIDField(raw json.RawMessage, field string, issues *[]fieldIssue) (uuid.UUID, bool) {
	if isNullOrAbsent(raw) {
		*issues = append(*issues, fieldIssue{Field: field, Issue: "is required"})
		return uuid.Nil, false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		*issues = append(*issues, fieldIssue{Field: field, Issue: "must be a string"})
		return uuid.Nil, false
	}
	id, err := parseCanonicalUUID(text)
	if err != nil {
		*issues = append(*issues, fieldIssue{Field: field, Issue: err.Error()})
		return uuid.Nil, false
	}
	return id, true
}

func parseMoneyField(raw json.RawMessage, field string, issues *[]fieldIssue) (money.Money, bool) {
	if isNullOrAbsent(raw) {
		*issues = append(*issues, fieldIssue{Field: field, Issue: "is required"})
		return money.Money{}, false
	}
	var m money.Money
	if err := json.Unmarshal(raw, &m); err != nil {
		*issues = append(*issues, fieldIssue{Field: field, Issue: err.Error()})
		return money.Money{}, false
	}
	return m, true
}

var errNotCanonicalUUID = errors.New("must be a UUID in the form xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx, and not the nil UUID")

// parseCanonicalUUID accepts only the 36-character form with dashes: the library
// also takes braces, a urn: prefix and no dashes, and one identifier should have
// one spelling.
func parseCanonicalUUID(text string) (uuid.UUID, error) {
	if len(text) != 36 {
		return uuid.Nil, errNotCanonicalUUID
	}
	id, err := uuid.Parse(text)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, errNotCanonicalUUID
	}
	return id, nil
}
