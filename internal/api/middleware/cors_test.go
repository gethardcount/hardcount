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

// preflight is the request headers a browser sends before a cross-origin POST
// with a JSON body and an Idempotency-Key.
var preflight = map[string]string{
	"Access-Control-Request-Method":  "POST",
	"Access-Control-Request-Headers": "content-type,idempotency-key",
}

func TestCORS_Preflight(t *testing.T) {
	rec, reached := corsRun(t, []string{"https://app.velox.dev"}, http.MethodOptions, "https://app.velox.dev", preflight)

	if reached {
		t.Error("a preflight must be answered by CORS, not reach the handler")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.velox.dev" {
		t.Errorf("allow-origin: got %q", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("a listed origin must be allowed to send the sign-in cookie")
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("missing allow-methods header")
	}
	if rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("missing allow-headers header")
	}
}

func TestCORS_AllowedOrigin(t *testing.T) {
	rec, _ := corsRun(t, []string{"https://app.velox.dev"}, http.MethodGet, "https://app.velox.dev", nil)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.velox.dev" {
		t.Errorf("allow-origin: got %q", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("a listed origin must be allowed to send the sign-in cookie")
	}
}

func TestCORS_DisallowedOrigin(t *testing.T) {
	rec, _ := corsRun(t, []string{"https://app.velox.dev"}, http.MethodGet, "https://evil.com", nil)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("should not set allow-origin for disallowed origin")
	}
}

// A bare "*" allows any origin to read responses, but without the sign-in
// cookie: the answer is a literal "*" and no Allow-Credentials, which browsers
// refuse for a request sent with cookies. Echoing the origin with credentials
// let any site read a signed-in operator's data.
func TestCORS_Wildcard(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		headers map[string]string
	}{
		{"request", http.MethodGet, nil},
		{"preflight", http.MethodOptions, preflight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := corsRun(t, []string{"*"}, tc.method, "https://anything.com", tc.headers)

			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("allow-origin: got %q, want a literal *", got)
			}
			if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
				t.Errorf("allow-credentials: got %q, want none with *", got)
			}
		})
	}
}

// An OPTIONS request without Access-Control-Request-Method is not a preflight,
// so it gets the route's own answer.
func TestCORS_PlainOptionsReachesHandler(t *testing.T) {
	_, reached := corsRun(t, []string{"https://app.velox.dev"}, http.MethodOptions, "", nil)
	if !reached {
		t.Error("an OPTIONS request that is not a preflight must reach the handler")
	}
}

// An empty list allows no origin. The CORS library reads an empty list as
// "every origin", so this pins that Velox does not.
func TestCORS_NoOriginsAllowsNone(t *testing.T) {
	for _, origins := range [][]string{nil, {}} {
		rec, _ := corsRun(t, origins, http.MethodGet, "https://evil.com", nil)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("CORS(%v): allow-origin %q, want none", origins, got)
		}
	}
}

// An entry with a * inside it is refused at boot (config.ParseCORSOrigins). A
// caller that skips that check still gets an exact match, never a pattern:
// "h*" as a pattern would allow every https origin with the sign-in cookie.
func TestCORS_StarInsideEntryIsNotAPattern(t *testing.T) {
	for _, entry := range []string{"h*", "https://*.example.com"} {
		rec, _ := corsRun(t, []string{entry}, http.MethodGet, "https://sib.example.com", nil)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("CORS([%q]): allow-origin %q for https://sib.example.com, want none", entry, got)
		}
	}
}

func TestCORS_ExposesVeloxHeaders(t *testing.T) {
	rec, _ := corsRun(t, []string{"*"}, http.MethodGet, "https://app.velox.dev", nil)

	// Header names are case-insensitive, and browsers match this list that
	// way: X-RateLimit-Limit and X-Ratelimit-Limit are the same header.
	exposed := strings.Split(rec.Header().Get("Access-Control-Expose-Headers"), ",")
	for _, want := range []string{"Velox-Version", "Velox-Request-Id", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"} {
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
