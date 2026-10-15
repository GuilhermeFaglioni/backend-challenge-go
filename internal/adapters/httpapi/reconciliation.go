package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/auth"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/reconcile"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

// Reconciler checks a wallet's books (the reconcile use case).
type Reconciler interface {
	Execute(ctx context.Context, walletID uuid.UUID, mode reconcile.Mode) (reconcile.Result, error)
}

// Reconciliation serves the check of a wallet's books, for the internal service.
type Reconciliation struct {
	reconciler Reconciler
	log        *slog.Logger
}

func NewReconciliation(r Reconciler, log *slog.Logger) *Reconciliation {
	return &Reconciliation{reconciler: r, log: log}
}

func (h *Reconciliation) register(r *Routes) {
	r.Protected("POST /wallets/{walletId}/reconciliation", http.HandlerFunc(h.reconcile), auth.RoleInternal)
}

// reconciliationResponse is the shape of the contract. The difference is signed.
type reconciliationResponse struct {
	WalletID          uuid.UUID   `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
	Mode              string      `json:"mode"`
	ReadEntries       int64       `json:"readEntries"`
	CheckpointMatches *bool       `json:"checkpointMatches,omitempty"` // full checks of a wallet with a checkpoint only
}

// reconcile answers 200 even when the books do not agree: a divergence is what the check
// found, not a problem with the request.
func (h *Reconciliation) reconcile(w http.ResponseWriter, req *http.Request) {
	correlation := correlationID(w, req)
	id, err := parseCanonicalUUID(req.PathValue("walletId"))
	if err != nil {
		writeInvalid(w, []fieldIssue{{Field: "walletId", Issue: err.Error()}})
		return
	}
	mode := reconcile.Incremental
	switch full := req.URL.Query()["full"]; {
	case len(full) == 0 || (len(full) == 1 && full[0] == "false"):
	case len(full) == 1 && full[0] == "true":
		mode = reconcile.Full
	default:
		writeInvalid(w, []fieldIssue{{Field: "full", Issue: "must be true or false, given once"}})
		return
	}
	result, err := h.reconciler.Execute(req.Context(), id, mode)
	switch {
	case err == nil:
		body := reconciliationResponse{
			WalletID: result.WalletID, StoredBalance: result.Stored, CalculatedBalance: result.Calculated,
			Difference: result.Difference, Consistent: result.Consistent, CheckedEntries: result.CheckedEntries,
			Mode: result.Mode.String(), ReadEntries: result.ReadEntries,
		}
		if result.CheckpointChecked {
			matches := !result.CheckpointMismatch
			body.CheckpointMatches = &matches
		}
		writeJSON(w, http.StatusOK, body)
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
