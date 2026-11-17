package middleware

import (
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/session"
)

// CSRFGuard refuses a write a browser sent from another site, before the
// session cookie the browser attached on its own can act. CORS cannot do
// this: it decides whether a page may read an answer, not whether the
// request runs. A cross-site sign-in matters most, because sign-in sets the
// session cookie.
//
// GET, HEAD, OPTIONS and TRACE pass. Any other method passes or answers 403
// "cross-site request blocked", by the first rule that applies:
//
//   - A Bearer Authorization header with no session cookie passes: a browser
//     never attaches a key on its own. With a session cookie too, the rules
//     below apply, or a junk Bearer header would let the cookie through.
//   - Sec-Fetch-Site same-origin or none passes, and cross-site is refused.
//     same-site passes only from a listed origin, so a sibling subdomain is
//     refused.
//   - With no Sec-Fetch-Site, or a value it does not know, a listed Origin
//     passes and any other Origin is refused. A request with neither header
//     passes: every browser sends one of them on a cross-site write, and page
//     script cannot remove them, so it did not come from a browser.
//
// allowedOrigins is CORS_ALLOWED_ORIGINS, matched as CORS matches it. Mount
// the guard on every group that accepts the session cookie.
func CSRFGuard(allowedOrigins []string) func(http.Handler) http.Handler {
	allow := newOriginSet(allowedOrigins)
	return func(next http.Handler) http.Handler {
		return CrossSiteCheck{allow: allow, next: next}
	}
}

// CrossSiteCheck refuses a cross-site browser write. CSRFGuard makes it; its
// doc has the rule.
type CrossSiteCheck struct {
	allow originSet
	next  http.Handler
}

func (c CrossSiteCheck) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isSafeMethod(r.Method) || isExemptBearerRequest(r) {
		c.next.ServeHTTP(w, r)
		return
	}
	if isCrossSiteBrowserRequest(r, c.allow) {
		respond.Forbidden(w, r, "cross-site request blocked")
		return
	}
	c.next.ServeHTTP(w, r)
}

// isSafeMethod reports whether method is one of HTTP's safe methods, which
// change no state.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func isExemptBearerRequest(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	if c, err := r.Cookie(session.CookieName); err == nil && c.Value != "" {
		return false
	}
	return true
}

func isCrossSiteBrowserRequest(r *http.Request, allow originSet) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "same-site":
		return !allow.has(r.Header.Get("Origin"))
	case "cross-site":
		return true
	}
	if strings.TrimSpace(r.Header.Get("Origin")) == "" {
		return false
	}
	return !allow.has(r.Header.Get("Origin"))
}
