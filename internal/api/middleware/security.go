package middleware

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// SecurityHeaders adds standard security headers to all responses.
// These are defense-in-depth measures that enterprise security audits expect.
func SecurityHeaders() func(http.Handler) http.Handler {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// HSTS: tell browsers to always use HTTPS (skip in local dev)
			if env != "local" {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}

			// Prevent MIME sniffing
			w.Header().Set("X-Content-Type-Options", "nosniff")

			// Prevent clickjacking
			w.Header().Set("X-Frame-Options", "DENY")

			// Disable client-side caching for API responses (financial data)
			w.Header().Set("Cache-Control", "no-store")

			// Referrer policy — don't leak URLs to third parties
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

			next.ServeHTTP(w, r)
		})
	}
}

// MetricsAuth protects the /metrics endpoint with a bearer token.
// Without METRICS_TOKEN the endpoint is open outside production, for local
// dev. In production it answers 401 to everyone: the scrape is operational
// data and costs database queries, and an address allowlist in front of it
// stops holding once a load balancer makes every caller a private address.
// It refuses the scrape rather than the boot, so an install that does not
// scrape metrics keeps billing through the upgrade.
func MetricsAuth(next http.Handler) http.Handler {
	token := strings.TrimSpace(os.Getenv("METRICS_TOKEN"))
	prod := strings.EqualFold(strings.TrimSpace(os.Getenv("APP_ENV")), "production")
	if token == "" && prod {
		slog.Warn("METRICS_TOKEN not set — /metrics answers 401 in production. Set METRICS_TOKEN and have Prometheus send it as a bearer token")
	}

	unauthorized := func(w http.ResponseWriter) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No token configured. Decided before the compare below, because an
		// empty bearer equals an empty token.
		if token == "" {
			if prod {
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Check Authorization: Bearer <token>, in constant time so the
		// response time does not depend on how much of the token matched.
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			unauthorized(w)
			return
		}

		next.ServeHTTP(w, r)
	})
}
