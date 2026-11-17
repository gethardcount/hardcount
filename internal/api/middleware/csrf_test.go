package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The guard's whole decision table: each row is a caller's shape and whether
// the guard lets it reach the handler. A refused request answers 403. The
// trusted origins are http://localhost:5173 and https://app.hardcount.dev.
func TestCSRFGuard_Matrix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		headers     map[string]string
		wantAllowed bool
	}{
		// The attacks: a modern browser names the cross-site initiator, and a
		// browser from before Sec-Fetch-Site still sends Origin.
		{"cross-site POST (modern browser) is blocked", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, false},
		{"cross-site POST via Origin fallback is blocked", http.MethodPost, map[string]string{"Origin": "https://evil.example"}, false},
		// A sandboxed iframe's opaque origin serializes as "null".
		{"Origin: null is blocked", http.MethodPost, map[string]string{"Origin": "null"}, false},

		// The dashboard: same-origin, or same-site from a trusted origin (the
		// dashboard on app.hardcount.dev calling api.hardcount.dev).
		{"same-origin POST (the dashboard itself) passes", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"same-site POST with allowlisted Origin passes", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://app.hardcount.dev"}, true},
		// A sibling subdomain is same-site but not the dashboard.
		{"same-site POST from a non-allowlisted sibling subdomain is blocked", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.hardcount.dev"}, false},
		// A typed address or a bookmark; page JS cannot forge it.
		{"Sec-Fetch-Site: none passes", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"same-origin POST via Origin fallback (no Sec-Fetch-Site) passes", http.MethodPost, map[string]string{"Origin": "http://localhost:5173"}, true},

		// Callers that are not browsers. A Bearer key is no ambient credential,
		// so a Bearer request with no session cookie is exempt even from a
		// cross-site origin. A Bearer header with a session cookie is the junk-
		// Bearer dodge, so it is origin-checked.
		{"bearer POST with no Origin passes", http.MethodPost, map[string]string{"Authorization": "Bearer hc_secret_abc"}, true},
		{"bearer POST is exempt even from a cross-site origin (no cookie)", http.MethodPost, map[string]string{"Authorization": "Bearer hc_secret_abc", "Sec-Fetch-Site": "cross-site"}, true},
		{"bearer + session cookie from cross-site is NOT exempt (blocked)", http.MethodPost, map[string]string{"Authorization": "Bearer x", "Cookie": "hardcount_session=abc", "Sec-Fetch-Site": "cross-site"}, false},
		{"bearer + session cookie same-origin passes (origin-checked, not blocked)", http.MethodPost, map[string]string{"Authorization": "Bearer x", "Cookie": "hardcount_session=abc", "Sec-Fetch-Site": "same-origin"}, true},
		// No browser header at all: a browser always sets one on a cross-site
		// write, so this is curl, an SDK or a server job.
		{"no browser headers at all (curl/SDK) passes", http.MethodPost, map[string]string{}, true},

		// The safe methods are never checked; every unsafe one is.
		{"cross-site GET is not gated (safe method)", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"cross-site HEAD is not gated (safe method)", http.MethodHead, map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"OPTIONS preflight is not gated", http.MethodOptions, map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"cross-site TRACE is not gated (safe method)", http.MethodTrace, map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"cross-site DELETE is blocked", http.MethodDelete, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"cross-site PATCH is blocked", http.MethodPatch, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"cross-site PUT is blocked", http.MethodPut, map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/v1/customers", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			reached := false
			rec := httptest.NewRecorder()
			CSRFGuard([]string{"http://localhost:5173", "https://app.hardcount.dev"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)
			if reached != tc.wantAllowed {
				t.Errorf("allowed = %v, want %v (status %d)", reached, tc.wantAllowed, rec.Code)
			}
			if !tc.wantAllowed && rec.Code != http.StatusForbidden {
				t.Errorf("blocked request returned %d, want 403", rec.Code)
			}
		})
	}
}
