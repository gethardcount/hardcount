package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// corsRun sends one request through CORS(origins) and reports the response and
// whether the next handler ran. headers are extra request headers, such as the
// Access-Control-Request-Method that makes an OPTIONS request a preflight.
func corsRun(t *testing.T, origins []string, method, origin string, headers map[string]string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	handler := CORS(origins)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, "/v1/customers", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, reached
}

// A listed origin may read the answer and send the sign-in cookie, on a plain
// request and on a preflight, which CORS answers itself.
func TestCORS_ListedOriginMaySendTheCookie(t *testing.T) {
	const origin = "https://app.hardcount.dev"
	preflight := map[string]string{
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "content-type,idempotency-key",
	}
	for _, tc := range []struct {
		name        string
		method      string
		headers     map[string]string
		wantStatus  int
		wantReached bool
	}{
		{"a plain request", http.MethodGet, nil, http.StatusOK, true},
		{"a preflight", http.MethodOptions, preflight, http.StatusNoContent, false},
	} {
		rec, reached := corsRun(t, []string{origin}, tc.method, origin, tc.headers)
		if rec.Code != tc.wantStatus || reached != tc.wantReached {
			t.Errorf("%s: status %d, handler reached %v; want %d, %v", tc.name, rec.Code, reached, tc.wantStatus, tc.wantReached)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("%s: allow-origin %q, want %q", tc.name, got, origin)
		}
		if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
			t.Errorf("%s: a listed origin must be allowed to send the sign-in cookie", tc.name)
		}
		if tc.method == http.MethodOptions && (rec.Header().Get("Access-Control-Allow-Methods") == "" || rec.Header().Get("Access-Control-Allow-Headers") == "") {
			t.Errorf("%s: missing allow-methods or allow-headers", tc.name)
		}
	}
}

// No list, an empty list, and an entry with a * inside it allow no unlisted
// origin: rs/cors reads an empty list as every origin, and "h*" as a pattern
// would allow every https origin with the sign-in cookie.
func TestCORS_NoOriginsAllowsNone(t *testing.T) {
	for _, origins := range [][]string{nil, {}, {"h*"}, {"https://*.example.com"}} {
		rec, _ := corsRun(t, origins, http.MethodGet, "https://sib.example.com", nil)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("CORS(%q): allow-origin %q, want none", origins, got)
		}
	}
}

func TestCORS_ExposesHardcountHeaders(t *testing.T) {
	rec, _ := corsRun(t, []string{"*"}, http.MethodGet, "https://app.hardcount.dev", nil)

	// Browsers match header names in this list case-insensitively.
	exposed := strings.Split(rec.Header().Get("Access-Control-Expose-Headers"), ",")
	for _, want := range []string{"Hardcount-Version", "Hardcount-Request-Id", "Idempotent-Replayed", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"} {
		found := false
		for _, h := range exposed {
			if strings.EqualFold(strings.TrimSpace(h), want) {
				found = true
			}
		}
		if !found {
			t.Errorf("should expose %s header, got %q", want, exposed)
		}
	}
}
