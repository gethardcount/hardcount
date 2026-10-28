package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The trusted dashboard origins, as CSRFGuard would receive them from
// CORS_ALLOWED_ORIGINS.
var testOrigins = []string{"http://localhost:5173", "https://app.hardcount.dev"}

// runGuard sends one request through CSRFGuard and reports whether it was
// allowed to reach the wrapped handler (true) or rejected with 403 (false).
func runGuard(t *testing.T, r *http.Request) (allowed bool, status int) {
	t.Helper()
	reached := false
	h := CSRFGuard(testOrigins)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return reached, w.Code
}

// TestCSRFGuard_Matrix pins the whole decision table. Each row states the
// browser/caller shape and whether the guard must let it through.
func TestCSRFGuard_Matrix(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		headers     map[string]string
		wantAllowed bool
	}{
		// --- the attacks these guard against ---
		{
			// The demonstrated login-fixation / logout-CSRF vector: a modern
			// browser announces the cross-site origin and cannot be told not to.
			name:        "cross-site POST (modern browser) is blocked",
			method:      http.MethodPost,
			headers:     map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
			wantAllowed: false,
		},
		{
			// Pre-2020 browser with no Sec-Fetch-Site still sends Origin on a
			// cross-site POST; the allowlist catches it.
			name:        "cross-site POST via Origin fallback is blocked",
			method:      http.MethodPost,
			headers:     map[string]string{"Origin": "https://evil.example"},
			wantAllowed: false,
		},
		{
			// A sandboxed iframe / opaque origin serializes as the string "null";
			// it is not the dashboard, so it is untrusted.
			name:        "Origin: null is blocked",
			method:      http.MethodPost,
			headers:     map[string]string{"Origin": "null"},
			wantAllowed: false,
		},

		// --- the legitimate dashboard ---
		{
			name:        "same-origin POST (the dashboard itself) passes",
			method:      http.MethodPost,
			headers:     map[string]string{"Sec-Fetch-Site": "same-origin"},
			wantAllowed: true,
		},
		{
			// Dashboard on app.hardcount.dev calling api.hardcount.dev is same-SITE, not
			// same-origin — must pass WHEN its Origin is an allowlisted dashboard
			// origin, or the real product breaks.
			name:        "same-site POST with allowlisted Origin passes",
			method:      http.MethodPost,
			headers:     map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://app.hardcount.dev"},
			wantAllowed: true,
		},
		{
			// A sibling subdomain (subdomain takeover / user-content host) is
			// same-site but NOT an allowlisted dashboard origin — it must not ride
			// the same-site label + Lax cookie into a mutation.
			name:        "same-site POST from a non-allowlisted sibling subdomain is blocked",
			method:      http.MethodPost,
			headers:     map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.hardcount.dev"},
			wantAllowed: false,
		},
		{
			// A user-initiated top-level action (bookmark/typed) is never
			// attacker-initiated; page JS cannot forge Sec-Fetch-Site: none.
			name:        "Sec-Fetch-Site: none passes",
			method:      http.MethodPost,
			headers:     map[string]string{"Sec-Fetch-Site": "none"},
			wantAllowed: true,
		},
		{
			name:        "same-origin POST via Origin fallback (no Sec-Fetch-Site) passes",
			method:      http.MethodPost,
			headers:     map[string]string{"Origin": "http://localhost:5173"},
			wantAllowed: true,
		},

		// --- non-browser callers (curl / SDK / server) ---
		{
			// Bearer is a non-ambient credential: CSRF-immune, and legitimately
			// sends no Origin. Even a bearer request with a cross-site Origin is
			// exempt (an attacker can't attach a victim's bearer key anyway).
			name:        "bearer POST with no Origin passes",
			method:      http.MethodPost,
			headers:     map[string]string{"Authorization": "Bearer hc_secret_abc"},
			wantAllowed: true,
		},
		{
			name:        "bearer POST is exempt even from a cross-site origin (no cookie)",
			method:      http.MethodPost,
			headers:     map[string]string{"Authorization": "Bearer hc_secret_abc", "Sec-Fetch-Site": "cross-site"},
			wantAllowed: true,
		},
		{
			// The fake-Bearer dodge: a cross-site attacker adds a junk bearer to
			// trip the exemption while relying on the ambient cookie (which takes
			// precedence in auth) to authenticate. Bearer + cookie is not the pure
			// SDK shape, so it is origin-checked, not exempted → blocked.
			name:        "bearer + session cookie from cross-site is NOT exempt (blocked)",
			method:      http.MethodPost,
			headers:     map[string]string{"Authorization": "Bearer x", "Cookie": "hardcount_session=abc", "Sec-Fetch-Site": "cross-site"},
			wantAllowed: false,
		},
		{
			// The same bearer+cookie request from the dashboard itself is fine —
			// origin-checking it just passes on same-origin.
			name:        "bearer + session cookie same-origin passes (origin-checked, not blocked)",
			method:      http.MethodPost,
			headers:     map[string]string{"Authorization": "Bearer x", "Cookie": "hardcount_session=abc", "Sec-Fetch-Site": "same-origin"},
			wantAllowed: true,
		},
		{
			// curl / a server job: no browser headers at all. A browser cannot
			// produce this on a cross-site request (it always sets at least one),
			// so "neither present" is not a CSRF shape — allow it, don't break
			// scripting. It still hits auth downstream.
			name:        "no browser headers at all (curl/SDK) passes",
			method:      http.MethodPost,
			headers:     map[string]string{},
			wantAllowed: true,
		},

		// --- safe methods are never gated ---
		{
			name:        "cross-site GET is not gated (safe method)",
			method:      http.MethodGet,
			headers:     map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantAllowed: true,
		},
		{
			name:        "OPTIONS preflight is not gated",
			method:      http.MethodOptions,
			headers:     map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantAllowed: true,
		},
		// --- every unsafe method is covered, not just POST ---
		{
			name:        "cross-site DELETE is blocked",
			method:      http.MethodDelete,
			headers:     map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantAllowed: false,
		},
		{
			name:        "cross-site PATCH is blocked",
			method:      http.MethodPatch,
			headers:     map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantAllowed: false,
		},
		{
			name:        "cross-site PUT is blocked",
			method:      http.MethodPut,
			headers:     map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantAllowed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/v1/customers", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			allowed, status := runGuard(t, req)
			if allowed != tc.wantAllowed {
				t.Errorf("allowed = %v, want %v (status %d)", allowed, tc.wantAllowed, status)
			}
			if !tc.wantAllowed && status != http.StatusForbidden {
				t.Errorf("blocked request returned %d, want 403", status)
			}
		})
	}
}

// TestCSRFGuard_ReproducesTheLoginFixation is the named regression lock for the
// vulnerability verified live: an attacker's cross-site POST to /v1/auth/login,
// which (pre-guard) minted the attacker's session in the victim's browser.
func TestCSRFGuard_ReproducesTheLoginFixation(t *testing.T) {
	// The browser sends this shape for a form on evil.example POSTing to the
	// dashboard: it stamps the real initiator, and page JS cannot change it.
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "http://127.0.0.1:5175")
	req.Header.Set("Content-Type", "text/plain") // the form-smuggled-JSON trick

	allowed, status := runGuard(t, req)
	if allowed {
		t.Fatal("cross-site POST to /v1/auth/login reached the handler — it could mint the attacker's session cookie in the victim's browser (login fixation)")
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
}

func TestIsSafeMethod(t *testing.T) {
	safe := []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace}
	unsafe := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	for _, m := range safe {
		if !isSafeMethod(m) {
			t.Errorf("%s should be safe", m)
		}
	}
	for _, m := range unsafe {
		if isSafeMethod(m) {
			t.Errorf("%s should be unsafe", m)
		}
	}
}
