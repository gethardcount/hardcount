package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMetricsAuth pins who may scrape /metrics. The production rows without a
// token are the point: an install that never set METRICS_TOKEN must answer
// 401, not serve the scrape to anyone who can reach the port. The empty-bearer
// row is there because an empty credential equals an empty token under any
// string compare, so "no token configured" has to be refused before comparing.
func TestMetricsAuth(t *testing.T) {
	const token = "s3cret-scrape-token"
	cases := []struct {
		name     string
		env      string
		token    string
		header   string
		wantCode int
	}{
		{"local without a token is open", "local", "", "", http.StatusOK},
		{"unset APP_ENV without a token is open", "", "", "", http.StatusOK},
		{"staging without a token is open", "staging", "", "", http.StatusOK},

		{"production without a token refuses an anonymous scrape", "production", "", "", http.StatusUnauthorized},
		{"production without a token refuses an empty bearer", "production", "", "Bearer ", http.StatusUnauthorized},
		{"production without a token refuses any bearer", "production", "", "Bearer " + token, http.StatusUnauthorized},
		{"production is matched case-insensitively", " Production ", "", "", http.StatusUnauthorized},
		{"production with a blank token is still closed", "production", "   ", "", http.StatusUnauthorized},

		{"token set: no header", "local", token, "", http.StatusUnauthorized},
		{"token set: wrong bearer", "local", token, "Bearer wrong", http.StatusUnauthorized},
		{"token set: prefix of the token", "local", token, "Bearer " + token[:len(token)-1], http.StatusUnauthorized},
		{"token set: token plus a suffix", "local", token, "Bearer " + token + "x", http.StatusUnauthorized},
		{"token set: empty bearer", "local", token, "Bearer ", http.StatusUnauthorized},
		{"token set: wrong scheme", "local", token, "Basic " + token, http.StatusUnauthorized},
		{"token set: correct bearer", "local", token, "Bearer " + token, http.StatusOK},
		{"production with a token: correct bearer", "production", token, "Bearer " + token, http.StatusOK},
		{"production with a token: no header", "production", token, "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("APP_ENV", tc.env)
			t.Setenv("METRICS_TOKEN", tc.token)

			served := false
			h := MetricsAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if wantServed := tc.wantCode == http.StatusOK; served != wantServed {
				t.Fatalf("scrape served = %v, want %v", served, wantServed)
			}
			if tc.wantCode == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("a 401 must carry WWW-Authenticate: Bearer, got %q", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}
