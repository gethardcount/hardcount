// Package middleware provides HTTP middleware for the Velox API server.
// Includes rate limiting, idempotency keys, Prometheus metrics, CORS, and
// cursor-based pagination helpers.
package middleware

import (
	"net/http"
	"slices"
	"strings"

	"github.com/rs/cors"
)

// CORS lets browser pages on the allowed origins call the API and read the
// response. github.com/rs/cors answers preflights and sets the headers; Velox
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
// The match is ours, not rs/cors's AllowedOrigins, because that option reads
// an empty list as every origin and an entry with a * inside it as a
// prefix-and-suffix pattern ("h*" would allow every https origin). The boot
// check refuses such entries; here they simply match nothing.
//
// An OPTIONS request without Access-Control-Request-Method is not a preflight
// and goes on to the router.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	opts := cors.Options{
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		AllowedHeaders: []string{"Authorization", "Content-Type", "X-API-Key", "Idempotency-Key"},
		ExposedHeaders: []string{"Velox-Version", "Velox-Request-Id", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"},
		MaxAge:         86400,
	}
	if slices.Contains(allowedOrigins, "*") {
		opts.AllowedOrigins = []string{"*"}
	} else {
		allow := make(map[string]bool, len(allowedOrigins))
		for _, o := range allowedOrigins {
			allow[strings.ToLower(o)] = true
		}
		opts.AllowOriginFunc = func(origin string) bool { return allow[strings.ToLower(origin)] }
		opts.AllowCredentials = true
	}
	return cors.New(opts).Handler
}
