// Package middleware holds the HTTP middleware of the API server. "The path
// of a request" in docs/architecture.md says where each one runs and why.
// The steps the router's chain test in internal/api checks have exported
// handler types, such as CrossSiteCheck: the test tells them apart by type.
package middleware

import (
	"net/http"
	"slices"
	"strings"

	"github.com/rs/cors"
)

// CORS lets browser pages on the allowed origins call the API and read its
// answers. github.com/rs/cors answers preflights and sets the headers;
// Hardcount decides which origins match. allowedOrigins comes from
// config.ParseCORSOrigins:
//
//   - A bare "*" allows every origin without credentials, so browsers send
//     no sign-in cookie: another site's page can call the API with a key but
//     cannot read a signed-in operator's data.
//   - Otherwise each listed origin matches as in CSRFGuard (originSet) and
//     may send the sign-in cookie, so a dashboard on another address can
//     sign in.
//
// Hardcount matches origins itself because rs/cors's AllowedOrigins reads an
// empty list as every origin, and an entry with a * inside as a pattern:
// "h*" would allow every https origin. The boot check refuses such entries,
// and here they match nothing.
//
// An OPTIONS request without Access-Control-Request-Method is not a
// preflight and goes on to the router.
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
