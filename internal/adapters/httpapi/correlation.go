package httpapi

import (
	"net/http"

	"github.com/google/uuid"
)

const (
	correlationHeader = "X-Correlation-Id"
	maxCorrelationLen = 128
)

// correlationID returns the id that follows the request through logs and events and
// puts it in the response. The client's own id is kept when it is a single header of
// 1 to 128 printable ASCII characters; otherwise a UUIDv7 is generated. An id that
// is not acceptable is replaced, not rejected: it is a convenience, not an input to
// be strict about.
func correlationID(w http.ResponseWriter, r *http.Request) string {
	id := ""
	if values := r.Header.Values(correlationHeader); len(values) == 1 && acceptableCorrelationID(values[0]) {
		id = values[0]
	}
	if id == "" {
		id = uuid.Must(uuid.NewV7()).String()
	}
	w.Header().Set(correlationHeader, id)
	return id
}

func acceptableCorrelationID(s string) bool {
	if s == "" || len(s) > maxCorrelationLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
