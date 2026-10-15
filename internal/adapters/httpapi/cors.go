package httpapi

import (
	"net/http"
	"slices"
)

// corsPolicy lets the Swagger UI, served from another origin, call the API from the
// browser. It is off unless origins are configured, matches origins exactly (never
// `*`), and sends no credentials-related headers: the API uses bearer tokens, not
// cookies.
type corsPolicy struct {
	origins []string
}

const (
	corsMethods       = "GET, POST, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, X-Correlation-Id"
	corsExposeHeaders = "X-Correlation-Id, Location, Retry-After, WWW-Authenticate"
	corsMaxAge        = "600"
)

func (p corsPolicy) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The answer depends on the Origin header, so caches must key on it.
		w.Header().Add("Vary", "Origin")

		origin := r.Header.Get("Origin")
		if origin == "" || !slices.Contains(p.origins, origin) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)

		// A preflight carries no credentials by design: it is answered here and
		// never reaches a route, and says nothing about the API's data.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", corsMethods)
			w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
			w.Header().Set("Access-Control-Max-Age", corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
