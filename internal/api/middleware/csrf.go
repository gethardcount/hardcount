package middleware

import (
	"net/http"
	"strings"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/session"
)

// CSRFGuard is the server-side CSRF defense for the cookie-authenticated
// dashboard surface. It rejects a state-changing request that a *browser*
// initiated from a site other than the trusted dashboard, before the request
// can spend the operator's ambient session cookie.
//
// # Why this exists (and why CORS did not already cover it)
//
// CSRF is an ambient-authority problem: the browser attaches hardcount_session to
// every request to the API automatically, so any page the operator visits can
// cause a request that carries it. SameSite=Lax narrows this (it withholds the
// cookie from most cross-site requests) but is not sufficient on its own — it
// still permits top-level GET navigation, and it does nothing about a cookie
// being *set* cross-site, which is how the login-fixation variant worked (an
// attacker's cross-site POST to /v1/auth/login minted and set THEIR session in
// the victim's browser). The existing CORS middleware is not a defense here at
// all: it governs whether the attacker's JS may *read the response*, never
// whether the request *executes* — a form POST or any "simple request" runs the
// handler regardless of CORS, and a login-fixation attacker never needs to read
// anything, the Set-Cookie side effect already happened.
//
// # The rule
//
// For unsafe methods (POST/PUT/PATCH/DELETE), unless the request carries an
// Authorization: Bearer header, the request must be same-site:
//
//   - A Bearer request is EXEMPT **only when it carries no session cookie**. A
//     bearer key is a non-ambient credential — an attacker cannot make a
//     victim's browser attach it — so the SDK/server-to-server path (bearer, no
//     cookie) is CSRF-immune and legitimately sends no Origin. But a request
//     bearing BOTH a bearer header AND a session cookie is not a legitimate
//     shape (the SDK has no cookie; the dashboard sends no bearer): it is the
//     "add a junk Authorization header to trip the exemption, let the ambient
//     cookie authenticate" move, which a CORS config that let the attacker's
//     origin send credentials would otherwise let through a preflight (CORS
//     `*` sends none; see CORS). Such a request is origin-checked, not
//     exempted, so the dodge fails regardless of CORS config.
//   - Otherwise the browser's Sec-Fetch-Site (which page JS cannot forge) is the
//     primary signal: same-origin and none pass; cross-site is rejected;
//     **same-site passes only if its Origin is an allowlisted dashboard origin**
//     — so a sibling subdomain (subdomain takeover, a user-content host) cannot
//     ride the same-site label plus the Lax cookie into a mutation. Sec-Fetch-
//     Site is absent only on pre-2020 browsers; there we fall back to the same
//     Origin allowlist (the CORS_ALLOWED_ORIGINS list, so the two can't drift).
//   - A request carrying NEITHER Sec-Fetch-Site NOR Origin is allowed. This is
//     deliberately not fail-closed: a genuine cross-site attack requires a
//     browser, and every browser that can run the dashboard sets at least one of
//     those headers on a cross-site unsafe request — page JS cannot remove them.
//     So "neither present" means the caller is not a browser (curl, an SDK, a
//     server job); it holds no ambient cookie and cannot be the CSRF vector.
//     Failing closed here would only break non-browser callers for no gain.
//
// This guard belongs ONLY on the dashboard-serving route groups (/v1/auth and
// the cookie-or-bearer /v1 group). It must not wrap the Stripe webhook group
// (HMAC-authenticated, server-to-server, no Origin) or the unauthenticated
// public payment/invoice surfaces (they carry no operator session to abuse).
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

// isSafeMethod reports whether the HTTP method is non-mutating. Safe methods are
// never gated: they change no state, and OPTIONS is the CORS preflight.
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// isExemptBearerRequest reports whether the request is the non-ambient
// (SDK / server-to-server) credential path that is immune to CSRF: a Bearer
// header AND no session cookie. A request carrying both a Bearer header and a
// session cookie is NOT exempted — that combination is the fake-Bearer dodge
// (see CSRFGuard), so it is origin-checked instead.
func isExemptBearerRequest(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	if c, err := r.Cookie(session.CookieName); err == nil && c.Value != "" {
		return false // ambient cookie present → not the pure-bearer path → origin-check it
	}
	return true
}

// isCrossSiteBrowserRequest decides whether an unsafe, non-exempt request came
// from a browser page on a site other than the trusted dashboard. See CSRFGuard
// for the full rationale on each branch, including why same-site is allowed only
// with an allowlisted Origin, and why "no signals at all" is allowed.
func isCrossSiteBrowserRequest(r *http.Request, allow map[string]bool) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return false
	case "same-site":
		// Same registrable domain, different origin. Trust it only if the Origin
		// is an allowlisted dashboard origin, so a sibling subdomain can't ride
		// the same-site label into a cookie-authenticated mutation.
		return !originAllowed(r, allow)
	case "cross-site":
		return true
	}
	// Sec-Fetch-Site absent (or an unrecognized value): fall back to Origin.
	// Absent Origin too → no browser provenance signal at all → not a browser
	// (curl/SDK) → not a CSRF vector, allow.
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
