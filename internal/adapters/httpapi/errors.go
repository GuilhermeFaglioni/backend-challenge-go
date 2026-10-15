package httpapi

import (
	"encoding/json"
	"net/http"
)

// Error codes of the API contract (ARCHITECTURE.md, section 11). They are stable:
// clients branch on them.
const (
	CodeUnauthenticated = "UNAUTHENTICATED"
	CodeForbidden       = "FORBIDDEN"
	CodeUnavailable     = "UNAVAILABLE"
)

// errorBody is the shape of every error response: {"error":{"code":...,"message":...}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
