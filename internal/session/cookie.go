package session

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// CookieConfig holds the session cookie's attributes that differ by install.
// Every session cookie is also HttpOnly, Path=/ and SameSite=Lax: Lax keeps
// it on top-level navigation to the dashboard while blocking most cross-site
// requests.
type CookieConfig struct {
	Domain string
	Secure bool
}

// DefaultCookieConfig returns the cookie attributes for appEnv, which is
// config.Config.Env.
//
// Secure is on in staging and production, for every request, so a
// browser sends the session cookie only over HTTPS, or to localhost
// (ADR-095 Phase 0(b)). Chrome and Firefox keep a Secure cookie at
// http://localhost, but Safari does not. Plain HTTP is for local, where
// Secure is off: make dev, and the Compose quickstart, which sets
// APP_ENV=local so Safari can sign in at http://localhost.
func DefaultCookieConfig(appEnv string) CookieConfig {
	return CookieConfig{
		Domain: strings.TrimSpace(os.Getenv("HARDCOUNT_COOKIE_DOMAIN")),
		Secure: appEnv == "production" || appEnv == "staging",
	}
}

// SetCookie writes the session cookie on the response. Sign-in
// (internal/user) and invite accept (internal/dashmembers) call it after
// IssueTx.
func (c CookieConfig) SetCookie(w http.ResponseWriter, raw string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    raw,
		Path:     "/",
		Domain:   c.Domain,
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie writes a cleared (Max-Age=-1) session cookie on the response,
// for sign-out.
func (c CookieConfig) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Domain:   c.Domain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}
