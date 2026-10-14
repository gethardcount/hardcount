package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Safari drops a Secure cookie set over plain http, even on localhost, so a
// production-mode install opened at http://localhost could not log in. The
// browser's Origin (or Referer) decides; Host does not, because a TLS proxy
// forwarding to localhost:8080 makes Host say localhost on an https site.
func TestSetCookie_SecureUnlessBrowserOnPlainHTTPLoopback(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		referer string
		host    string
		want    bool
	}{
		{name: "https site", origin: "https://billing.example.com", want: true},
		{name: "https site behind a proxy to localhost", origin: "https://billing.example.com", host: "localhost:8080", want: true},
		{name: "plain http public host", origin: "http://billing.example.com", want: true},
		{name: "no Origin or Referer", host: "localhost:8080", want: true},
		{name: "opaque Origin", origin: "null", want: true},
		{name: "https localhost", origin: "https://localhost:8443", want: true},
		{name: "http localhost", origin: "http://localhost:8099", want: false},
		{name: "http 127.0.0.1", origin: "http://127.0.0.1:8080", want: false},
		{name: "http [::1]", origin: "http://[::1]:8080", want: false},
		{name: "http app.localhost", origin: "http://velox.localhost", want: false},
		{name: "Referer when Origin is absent", referer: "http://localhost:8099/login", want: false},
		{name: "lookalike host", origin: "http://localhost.example.com", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
			if tc.host != "" {
				r.Host = tc.host
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.referer != "" {
				r.Header.Set("Referer", tc.referer)
			}
			c := CookieConfig{Secure: true, SameSite: http.SameSiteLaxMode, Path: "/"}
			for _, set := range []func(http.ResponseWriter){
				func(w http.ResponseWriter) { c.SetCookie(w, r, "raw", time.Now().Add(time.Hour)) },
				func(w http.ResponseWriter) { c.ClearCookie(w, r) },
			} {
				w := httptest.NewRecorder()
				set(w)
				got := w.Result().Cookies()
				if len(got) != 1 || got[0].Secure != tc.want {
					t.Fatalf("Secure = %v, want %v (cookies %v)", got[0].Secure, tc.want, got)
				}
			}
		})
	}

	// Local dev (APP_ENV not production) never sets Secure.
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	r.Header.Set("Origin", "https://billing.example.com")
	w := httptest.NewRecorder()
	CookieConfig{Secure: false}.SetCookie(w, r, "raw", time.Now().Add(time.Hour))
	if w.Result().Cookies()[0].Secure {
		t.Fatal("Secure set with CookieConfig.Secure=false")
	}
}
