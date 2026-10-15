package httpapi

import (
	"encoding/json"
	"net/http"
)

// Error codes of the API contract (ARCHITECTURE.md, section 11). They are stable:
// clients branch on them.
const (
	CodeInvalidRequest      = "INVALID_REQUEST"
	CodeUnauthenticated     = "UNAUTHENTICATED"
	CodeForbidden           = "FORBIDDEN"
	CodeNotFound            = "NOT_FOUND"
	CodeWalletAlreadyExists = "WALLET_ALREADY_EXISTS"
	CodeUnavailable         = "UNAVAILABLE"
	CodeInternal            = "INTERNAL_ERROR"
)

// errorBody is the shape of every error response:
// {"error":{"code":...,"message":...}} plus, where it applies, the fields at fault
// and the id of the wallet a conflict is about.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code     string       `json:"code"`
	Message  string       `json:"message"`
	Details  []fieldIssue `json:"details,omitempty"`
	WalletID string       `json:"walletId,omitempty"`
}

// fieldIssue says what is wrong with one field of a request.
type fieldIssue struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
