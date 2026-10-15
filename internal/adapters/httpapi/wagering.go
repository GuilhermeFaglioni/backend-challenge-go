package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

const maxIdempotencyKeyLength = 255

// OperationProcessor processes an external operation (the processwager use case).
type OperationProcessor interface {
	Execute(ctx context.Context, in processwager.Input) (processwager.Output, error)
}

// Wagering serves the endpoint providers send their operations to.
type Wagering struct {
	processor OperationProcessor
	log       *slog.Logger
}

func NewWagering(processor OperationProcessor, log *slog.Logger) *Wagering {
	return &Wagering{processor: processor, log: log}
}

func (h *Wagering) register(r *Routes) {
	r.Protected("POST /wagering/transactions", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.submit(r, w, req)
	}), auth.RoleProvider)
}

// operationBody holds the fields raw, so that each can be checked and reported on its
// own, and so that the provider can be looked at before anything else.
type operationBody struct {
	ProviderID                     json.RawMessage `json:"providerId"`
	ExternalTransactionID          json.RawMessage `json:"externalTransactionId"`
	PlayerID                       json.RawMessage `json:"playerId"`
	WalletID                       json.RawMessage `json:"walletId"`
	RoundID                        json.RawMessage `json:"roundId"`
	GameID                         json.RawMessage `json:"gameId"`
	Kind                           json.RawMessage `json:"kind"`
	Money                          json.RawMessage `json:"money"`
	ReferenceExternalTransactionID json.RawMessage `json:"referenceExternalTransactionId"`
}

type processedResponse struct {
	TransactionID    uuid.UUID   `json:"transactionId"`
	Status           string      `json:"status"`
	Balance          money.Money `json:"balance"`
	IdempotentReplay bool        `json:"idempotentReplay"`
}

type rejectedResponse struct {
	TransactionID    uuid.UUID         `json:"transactionId"`
	Status           string            `json:"status"`
	FailureCode      wager.FailureCode `json:"failureCode"`
	IdempotentReplay bool              `json:"idempotentReplay"`
}

func (h *Wagering) submit(r *Routes, w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	correlation := correlationID(w, req)

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok || principal.ProviderID() == "" {
		// The registry guarantees a provider principal; fail closed if it did not.
		r.refuse(ctx, w, req, auth.ReasonForbidden)
		return
	}

	var body operationBody
	if issues := decodeBody(w, req, &body); len(issues) > 0 {
		writeInvalid(w, issues)
		return
	}

	// Whose operation this is comes first: the provider named in the body must be the
	// one the token belongs to. Nothing else about a request that is not yours is read.
	var issues []fieldIssue
	provider, hasProvider := stringField(body.ProviderID, "providerId", &issues)
	if !hasProvider {
		writeInvalid(w, issues)
		return
	}
	if provider != principal.ProviderID() {
		r.refuse(ctx, w, req, auth.ReasonProviderMismatch)
		return
	}

	key, issue := idempotencyKey(req.Header)
	if issue != nil {
		writeInvalid(w, []fieldIssue{*issue})
		return
	}

	params, issues := h.params(body, principal.ProviderID())
	if len(issues) > 0 {
		writeInvalid(w, issues)
		return
	}
	if _, err := wager.NewRequest(params); err != nil {
		writeInvalid(w, issuesOf(err))
		return
	}

	out, err := h.processor.Execute(ctx, processwager.Input{Request: params, IdempotencyKey: key, CorrelationID: correlation})
	if err != nil {
		h.fail(w, req, correlation, err)
		return
	}
	h.answer(w, req, correlation, out)
}

// params reads the fields of the body. The provider is the token's.
func (h *Wagering) params(body operationBody, provider string) (wager.RequestParams, []fieldIssue) {
	var issues []fieldIssue
	p := wager.RequestParams{ProviderID: provider}
	p.ExternalTransactionID, _ = stringField(body.ExternalTransactionID, "externalTransactionId", &issues)
	p.PlayerID, _ = parseUUIDField(body.PlayerID, "playerId", &issues)
	p.WalletID, _ = parseUUIDField(body.WalletID, "walletId", &issues)
	p.RoundID, _ = stringField(body.RoundID, "roundId", &issues)
	p.GameID, _ = stringField(body.GameID, "gameId", &issues)
	kind, _ := stringField(body.Kind, "kind", &issues)
	p.Kind = wager.Kind(kind)
	p.Amount, _ = parseMoneyField(body.Money, "money", &issues)
	if !isNullOrAbsent(body.ReferenceExternalTransactionID) {
		p.ReferenceExternalTransactionID, _ = stringField(body.ReferenceExternalTransactionID, "referenceExternalTransactionId", &issues)
	}
	return p, issues
}

// answer renders a stored transaction: 200 with the balance when processed, 422 with
// the failure code when a rule refused it.
func (h *Wagering) answer(w http.ResponseWriter, req *http.Request, correlation string, out processwager.Output) {
	tx := out.Transaction
	switch tx.Status() {
	case wager.StatusProcessed:
		result, _ := tx.Result()
		writeJSON(w, http.StatusOK, processedResponse{
			TransactionID: tx.ID(), Status: string(wager.StatusProcessed), Balance: result.Balance, IdempotentReplay: out.IdempotentReplay,
		})
	case wager.StatusRejected:
		writeJSON(w, http.StatusUnprocessableEntity, rejectedResponse{
			TransactionID: tx.ID(), Status: string(wager.StatusRejected), FailureCode: tx.FailureCode(), IdempotentReplay: out.IdempotentReplay,
		})
	default:
		h.log.ErrorContext(req.Context(), "an operation ended in a state this endpoint cannot answer",
			slog.String("correlationId", correlation), slog.String("transactionId", tx.ID().String()), slog.String("status", string(tx.Status())))
		writeError(w, http.StatusInternalServerError, CodeInternal, "an unexpected error occurred; quote the correlation id when reporting it")
	}
}

func (h *Wagering) fail(w http.ResponseWriter, req *http.Request, correlation string, err error) {
	switch {
	case errors.Is(err, processwager.ErrIdempotencyKeyConflict):
		writeError(w, http.StatusConflict, CodeIdempotencyKeyConflict, "the idempotency key was already used with a different request")
	case errors.Is(err, processwager.ErrDuplicateExternalTransaction):
		writeError(w, http.StatusConflict, CodeDuplicateExternalTransaction, "the external transaction id was already used with another idempotency key")
	case errors.Is(err, processwager.ErrInvalidInput):
		writeInvalid(w, issuesOf(err))
	case errors.Is(err, application.ErrTemporarilyUnavailable):
		w.Header().Set("Retry-After", retryAfterTransient)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "a dependency is temporarily unavailable, try again shortly with the same Idempotency-Key")
	default:
		h.log.ErrorContext(req.Context(), "request failed",
			slog.String("correlationId", correlation), slog.String("path", req.URL.Path), slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, CodeInternal, "an unexpected error occurred; quote the correlation id when reporting it")
	}
}

// --- parsing ---------------------------------------------------------------------

// idempotencyKey reads the required header, exactly one value of 1 to 255 printable
// ASCII characters without spaces, as it is, never replaced.
func idempotencyKey(header http.Header) (string, *fieldIssue) {
	values := header.Values("Idempotency-Key")
	issue := func(text string) (string, *fieldIssue) {
		return "", &fieldIssue{Field: "Idempotency-Key", Issue: text}
	}
	switch len(values) {
	case 0:
		return issue("is required")
	case 1:
	default:
		return issue("must be sent once")
	}
	key := values[0]
	if key == "" || len(key) > maxIdempotencyKeyLength {
		return issue("must be 1 to 255 characters")
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return issue("must be printable ASCII without spaces")
		}
	}
	return key, nil
}

// stringField reads a field that must be a JSON string.
func stringField(raw json.RawMessage, field string, issues *[]fieldIssue) (string, bool) {
	if isNullOrAbsent(raw) {
		*issues = append(*issues, fieldIssue{Field: field, Issue: "is required"})
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		*issues = append(*issues, fieldIssue{Field: field, Issue: "must be a string"})
		return "", false
	}
	return text, true
}

// issuesOf says which field a domain error is about.
func issuesOf(err error) []fieldIssue {
	field := "body"
	switch {
	case errors.Is(err, wager.ErrInvalidProvider):
		field = "providerId"
	case errors.Is(err, wager.ErrInvalidIdentifier):
		// The error names the field after its last colon.
		if i := strings.LastIndex(err.Error(), ": "); i >= 0 {
			field = err.Error()[i+2:]
		}
	case errors.Is(err, wager.ErrInvalidKind):
		field = "kind"
	case errors.Is(err, wager.ErrInvalidAmount):
		field = "money"
	case errors.Is(err, wager.ErrReferenceRequired), errors.Is(err, wager.ErrReferenceNotAllowed):
		field = "referenceExternalTransactionId"
	}
	return []fieldIssue{{Field: field, Issue: err.Error()}}
}
