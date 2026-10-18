package session

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// CookieConfig centralises cookie attributes. SameSite=Lax keeps the
// cookie attached across top-level navigation (correct for a first-party
// dashboard) while blocking most cross-site CSRF.
type CookieConfig struct {
	Domain   string
	Secure   bool
	SameSite http.SameSite
	Path     string
}

// DefaultCookieConfig reads APP_ENV and returns a sensible default.
// Tests override fields directly.
//
// Secure is on in staging and production, for every request, so a
// browser sends the session cookie only over HTTPS, or to localhost
// (ADR-095 Phase 0(b)). Chrome and Firefox keep a Secure cookie at
// http://localhost; Safari does not. Plain HTTP is for local, where
// Secure is off: make dev, and the Compose quickstart, which sets
// APP_ENV=local so Safari can sign in at http://localhost.
func DefaultCookieConfig() CookieConfig {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	secure := env == "production" || env == "staging"
	return CookieConfig{
		Domain:   strings.TrimSpace(os.Getenv("VELOX_COOKIE_DOMAIN")),
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
	}
}

// SetCookie writes the session cookie on the response. Sign-in
// (internal/user) and invite accept (internal/dashmembers) call it after
// Issue.
func (c CookieConfig) SetCookie(w http.ResponseWriter, raw string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    raw,
		Path:     c.Path,
		Domain:   c.Domain,
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}

// ClearCookie writes a cleared (Max-Age=-1) cookie on the response.
// Used by the logout handler.
func (c CookieConfig) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     c.Path,
		Domain:   c.Domain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
	})
}
