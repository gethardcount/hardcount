package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// CORS_ALLOWED_ORIGINS written with a space after the comma ("a, b") must
// allow both origins. The CORS middleware matches entries exactly, so an
// untrimmed " b" entry never matched and that dashboard origin was refused.
func TestCORSAllowedOrigins_EntriesAreTrimmed(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://a.example.com, https://b.example.com ,")
	srv := newTestServer()

	allowOrigin := func(origin string) string {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Header().Get("Access-Control-Allow-Origin")
	}

	for _, origin := range []string{"https://a.example.com", "https://b.example.com"} {
		if got := allowOrigin(origin); got != origin {
			t.Errorf("origin %q: Access-Control-Allow-Origin = %q, want it echoed", origin, got)
		}
	}
	if got := allowOrigin("https://evil.example.com"); got != "" {
		t.Errorf("unlisted origin got Access-Control-Allow-Origin %q", got)
	}
	// The trailing comma must not become an allowlist entry: a request with
	// no Origin header would match the empty string and get CORS headers.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("request without Origin got Access-Control-Allow-Credentials %q", got)
	}
}

// CORS_ALLOWED_ORIGINS=* lets any site read responses, but not with the
// sign-in cookie: a page on another site must not read a signed-in
// operator's data.
func TestCORSAllowedOrigins_WildcardSendsNoCredentials(t *testing.T) {
	t.Setenv("CORS_ALLOWED_ORIGINS", "*")
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "https://sibling.example.com")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want a literal *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want none with *", got)
	}
}
