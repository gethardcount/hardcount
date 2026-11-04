package middleware

import (
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/session"
)

// CSRFGuard is the server-side CSRF defense for the dashboard routes that
// accept the session cookie. It rejects a state-changing request that a
// browser started from a site other than the trusted dashboard, before the
// request can use the operator's ambient session cookie.
//
// # Why this exists, and why CORS does not cover it
//
// CSRF is an ambient-authority problem. The browser attaches
// hardcount_session to every request to the API on its own, so any page the
// operator visits can send a request that carries it. SameSite=Lax withholds
// the cookie from most cross-site requests, but it is not enough. It still
// allows top-level GET navigation, and it does nothing about a cookie set
// cross-site. That is login fixation: a cross-site POST to /v1/auth/login
// sets the attacker's new session in the victim's browser.
//
// CORS is no defense here. It decides whether the attacker's JS may read the
// response, never whether the request runs. A form POST, or any "simple
// request", runs the handler whatever CORS says. A login-fixation attacker
// needs to read nothing: the Set-Cookie side effect has already happened.
//
// # The rule
//
// An unsafe request (POST, PUT, PATCH, DELETE) must be same-site, unless it
// carries an Authorization: Bearer header:
//
//   - A Bearer request is exempt only when it carries no session cookie. A
//     bearer key is not an ambient credential: an attacker cannot make a
//     victim's browser attach it. So the SDK and server-to-server path
//     (bearer, no cookie) is immune to CSRF, and rightly sends no Origin. A
//     request with both a bearer header and a session cookie is not a real
//     shape: the SDK has no cookie, and the dashboard sends no bearer. It is
//     a dodge: add a junk Authorization header to trip the exemption, and
//     let the ambient cookie authenticate. A CORS config that let the
//     attacker's origin send credentials would pass it through a preflight
//     (CORS `*` sends none; see CORS). So the guard origin-checks such a
//     request instead of exempting it, and the dodge fails whatever the CORS
//     config.
//   - Otherwise the browser's Sec-Fetch-Site header is the main signal, and
//     page JS cannot forge it. same-origin and none pass, and cross-site is
//     rejected. same-site passes only when its Origin is an allowlisted
//     dashboard origin. That stops a sibling subdomain (a subdomain takeover,
//     a user-content host) from using the same-site label and the Lax cookie
//     to make a mutation. Only browsers from before 2020 omit Sec-Fetch-Site.
//     For them the guard falls back to the same Origin allowlist, the
//     CORS_ALLOWED_ORIGINS list, so the two cannot drift apart.
//   - A request with neither Sec-Fetch-Site nor Origin is allowed: the guard
//     does not fail closed here, on purpose. A real cross-site attack needs a
//     browser, and every browser that can run the dashboard sets at least one
//     of those headers on a cross-site unsafe request. Page JS cannot remove
//     them. So "neither present" means the caller is not a browser (curl, an
//     SDK, a server job). It holds no ambient cookie and cannot be the CSRF
//     vector. Failing closed would only break callers that are not browsers.
//
// Mount it only on the route groups that serve the dashboard: /v1/auth, and
// the /v1 group that takes a cookie or a bearer key. Keep it off the Stripe
// webhook group (HMAC-authenticated, server to server, no Origin). Keep it off
// the public payment and invoice pages too: they need no sign-in, so they
// carry no operator session to abuse.
func CSRFGuard(allowedOrigins []string) func(http.Handler) http.Handler {
	allow := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" && o != "*" {
			allow[o] = true
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) || isExemptBearerRequest(r) {
				next.ServeHTTP(w, r)
				return
			}
			if isCrossSiteBrowserRequest(r, allow) {
				respond.Forbidden(w, r, "cross-site request blocked")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isSafeMethod reports whether the HTTP method changes no state. The guard
// never checks these methods, and OPTIONS is the CORS preflight.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// isExemptBearerRequest reports whether the request uses the non-ambient
// credential path (SDK, server to server) that is immune to CSRF: a Bearer
// header and no session cookie. A request with both a Bearer header and a
// session cookie is not exempt. That pair is the fake-Bearer dodge (see
// CSRFGuard), so it is origin-checked instead.
func isExemptBearerRequest(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	if c, err := r.Cookie(session.CookieName); err == nil && c.Value != "" {
		return false // ambient cookie present → not the pure-bearer path → origin-check it
	}
	return true
}

// isCrossSiteBrowserRequest reports whether an unsafe, non-exempt request came
// from a browser page on a site other than the trusted dashboard. CSRFGuard
// gives the reason for each branch: why same-site needs an allowlisted
// Origin, and why a request with no signal at all is allowed.
func isCrossSiteBrowserRequest(r *http.Request, allow map[string]bool) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "same-site":
		// Same registrable domain, different origin. Trust it only when the
		// Origin is an allowlisted dashboard origin, so a sibling subdomain
		// cannot use the same-site label for a cookie-authenticated mutation.
		return !originAllowed(r, allow)
	case "cross-site":
		return true
	}
	// Sec-Fetch-Site is absent or unrecognized, so fall back to Origin. With
	// no Origin either, nothing shows a browser sent it. The caller is curl
	// or an SDK, not a CSRF vector, so allow it.
	if strings.TrimSpace(r.Header.Get("Origin")) == "" {
		return false
	}
	return !originAllowed(r, allow)
}

// originAllowed reports whether the request's Origin header exactly matches a
// trusted dashboard origin (case-insensitive). An absent or "null" Origin is
// not allowed.
func originAllowed(r *http.Request, allow map[string]bool) bool {
	origin := strings.ToLower(strings.TrimSpace(r.Header.Get("Origin")))
	return origin != "" && allow[origin]
}
