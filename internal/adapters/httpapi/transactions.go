package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

// timeLayout is RFC 3339 in UTC with milliseconds, like the events.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// Transactions serves the reads of Wager Transactions. A provider reaches only its own;
// the internal service reaches all of them.
type Transactions struct {
	reader application.TransactionReader
	log    *slog.Logger
}

func NewTransactions(reader application.TransactionReader, log *slog.Logger) *Transactions {
	return &Transactions{reader: reader, log: log}
}

func (h *Transactions) register(r *Routes) {
	roles := []auth.Role{auth.RoleProvider, auth.RoleInternal}
	r.Protected("GET /wagering/transactions/{transactionId}", http.HandlerFunc(h.byID), roles...)
	r.Protected("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.byExternalID(r, w, req)
	}), roles...)
}

// transactionView is a transaction as a reader sees it. It leaves out what belongs to
// the writer: the idempotency key, the payload hash, the correlation id.
type transactionView struct {
	TransactionID                  uuid.UUID    `json:"transactionId"`
	Status                         string       `json:"status"`
	Kind                           string       `json:"kind"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	PlayerID                       uuid.UUID    `json:"playerId"`
	WalletID                       uuid.UUID    `json:"walletId"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	Money                          money.Money  `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	Balance                        *money.Money `json:"balance,omitempty"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	CreatedAt                      string       `json:"createdAt"`
	UpdatedAt                      string       `json:"updatedAt"`
	ProcessedAt                    string       `json:"processedAt,omitempty"`
}

func viewOf(t wager.Transaction) transactionView {
	external := t.External()
	view := transactionView{
		TransactionID: t.ID(), Status: string(t.Status()), Kind: string(t.Kind()),
		ProviderID: external.ProviderID, ExternalTransactionID: external.ExternalTransactionID,
		PlayerID: t.PlayerID(), WalletID: t.RequestedWalletID(), RoundID: external.RoundID, GameID: external.GameID,
		Money: t.Amount(), ReferenceExternalTransactionID: external.ReferenceExternalTransactionID,
		FailureCode: string(t.FailureCode()),
		CreatedAt:   t.CreatedAt().UTC().Format(timeLayout), UpdatedAt: t.UpdatedAt().UTC().Format(timeLayout),
	}
	if result, ok := t.Result(); ok {
		balance := result.Balance
		view.Balance = &balance
	}
	if at, ok := t.ProcessedAt(); ok {
		view.ProcessedAt = at.UTC().Format(timeLayout)
	}
	return view
}

// scopeOf says what the caller may read: the internal service everything, a provider
// only its own.
func scopeOf(p auth.Principal) application.Scope {
	if p.HasRole(auth.RoleInternal) {
		return application.AllTransactions()
	}
	return application.ProviderTransactions(p.ProviderID())
}

func (h *Transactions) byID(w http.ResponseWriter, req *http.Request) {
	correlation := correlationID(w, req)
	principal, ok := auth.PrincipalFrom(req.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthenticated, "authentication required")
		return
	}
	id, err := parseCanonicalUUID(req.PathValue("transactionId"))
	if err != nil {
		writeInvalid(w, []fieldIssue{{Field: "transactionId", Issue: err.Error()}})
		return
	}
	tx, err := h.reader.ByID(req.Context(), id, scopeOf(principal))
	h.answer(w, req, correlation, tx, err)
}

func (h *Transactions) byExternalID(r *Routes, w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	correlation := correlationID(w, req)
	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthenticated, "authentication required")
		return
	}
	provider := req.PathValue("providerId")
	// A provider asks under its own name. Another provider's namespace is not its to
	// read: refused before anything is looked up.
	if !principal.HasRole(auth.RoleInternal) && provider != principal.ProviderID() {
		r.refuse(ctx, w, req, auth.ReasonProviderMismatch)
		return
	}
	tx, err := h.reader.ByExternalID(ctx, provider, req.PathValue("externalTransactionId"))
	h.answer(w, req, correlation, tx, err)
}

func (h *Transactions) answer(w http.ResponseWriter, req *http.Request, correlation string, tx wager.Transaction, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, viewOf(tx))
	case errors.Is(err, application.ErrTransactionNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "there is no such transaction")
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
