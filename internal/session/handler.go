package session

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// CookieConfig centralises cookie attributes. Secure is pinned by
// APP_ENV: off for local (HTTP), on for staging/production (HTTPS).
// SameSite=Lax keeps the cookie attached across top-level navigation
// (correct for a first-party dashboard) while blocking most cross-site
// CSRF.
type CookieConfig struct {
	Domain   string
	Secure   bool
	SameSite http.SameSite
	Path     string
}

// DefaultCookieConfig reads APP_ENV and returns a sensible default.
// Tests override fields directly.
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

// SetCookie writes the session cookie on the response. Exported so the
// auth login handler in internal/userauth can call it after Issue
// without depending on internals here.
func (c CookieConfig) SetCookie(w http.ResponseWriter, r *http.Request, raw string, expires time.Time) {
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
		Secure:   c.secureFor(r),
		SameSite: c.SameSite,
	})
}

// ClearCookie writes a cleared (Max-Age=-1) cookie on the response.
// Used by the logout handler.
func (c CookieConfig) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     c.Path,
		Domain:   c.Domain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.secureFor(r),
		SameSite: c.SameSite,
	})
}

// secureFor keeps Secure except when the browser is on plain http at a
// loopback address. The quickstart runs APP_ENV=production at
// http://localhost, and Safari drops a Secure cookie set over http even on
// localhost (Chrome and Firefox keep it), so login silently failed there.
// The signal is the browser's own Origin (or Referer) header, not Host:
// behind a TLS proxy that forwards to localhost:8080, Host can say
// localhost while the browser is on https. A client that forges Origin
// only weakens its own cookie. Without either header, Secure stays on.
func (c CookieConfig) secureFor(r *http.Request) bool {
	if !c.Secure || r == nil {
		return c.Secure
	}
	src := r.Header.Get("Origin")
	if src == "" || src == "null" {
		src = r.Header.Get("Referer")
	}
	u, err := url.Parse(src)
	if err != nil || u.Scheme != "http" {
		return true
	}
	return !isLoopbackHost(u.Hostname())
}

func isLoopbackHost(h string) bool {
	h = strings.ToLower(h)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
