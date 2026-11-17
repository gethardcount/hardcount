// Package middleware provides HTTP middleware for the Hardcount API server.
// It includes rate limiting, idempotency keys, Prometheus metrics, CORS and
// cursor-based pagination helpers.
package middleware

import (
	"net/http"
	"slices"
	"strings"

	"github.com/rs/cors"
)

// CORS lets browser pages on the allowed origins call the API and read the
// response. github.com/rs/cors answers preflights and sets the headers; Hardcount
// only decides which origins match.
//
// allowedOrigins comes from config.ParseCORSOrigins, the parser behind the
// boot check. Two shapes:
//
//   - A bare "*" allows every origin, without credentials. The answer is a
//     literal "*" with no Access-Control-Allow-Credentials, so browsers send
//     no sign-in cookie: a page on another site can call the API with an API
//     key but cannot read a signed-in operator's data.
//   - Listed origins match exactly, ignoring case: the same rule as CSRFGuard,
//     which gets the same list. A listed origin may send the sign-in cookie,
//     so a dashboard served from another address can sign in.
//
// Hardcount matches origins itself instead of using rs/cors's AllowedOrigins.
// That option reads an empty list as every origin, and an entry with a *
// inside it as a prefix-and-suffix pattern: "h*" would allow every https
// origin. The boot check refuses such entries, and here they match nothing.
//
// An OPTIONS request without Access-Control-Request-Method is not a preflight
// and goes on to the router.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	opts := cors.Options{
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		AllowedHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key"},
		ExposedHeaders: []string{"Hardcount-Version", "Hardcount-Request-Id", "Idempotent-Replayed", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"},
		MaxAge:         86400,
	}
	if slices.Contains(allowedOrigins, "*") {
		opts.AllowedOrigins = []string{"*"}
	} else {
		opts.AllowOriginFunc = newOriginSet(allowedOrigins).has
		opts.AllowCredentials = true
	}
	return cors.New(opts).Handler
}

// originSet holds the listed origins, which CORS and CSRFGuard match alike:
// exactly, ignoring case. A "*" entry is no origin and matches nothing.
type originSet map[string]bool

func newOriginSet(origins []string) originSet {
	set := make(originSet, len(origins))
	for _, o := range origins {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" && o != "*" {
			set[o] = true
		}
	}
	return set
}

func (s originSet) has(origin string) bool {
	return s[strings.ToLower(strings.TrimSpace(origin))]
}
