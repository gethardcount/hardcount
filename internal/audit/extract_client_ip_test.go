package audit_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/sagarsuperuser/velox/internal/audit"
)

// TestExtractClientIP pins the security property: with no client IP resolved
// on the context (the global TrustedClientIP middleware sets one under its
// proxy-trust policy), the audit IP is the TCP peer in r.RemoteAddr and NEVER
// a raw X-Forwarded-For / X-Real-IP value — otherwise any client could forge
// audit_log.ip_address.
func TestExtractClientIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		xRealIP    string
		want       string
	}{
		{
			name:       "host:port → host",
			remoteAddr: "203.0.113.7:54321",
			want:       "203.0.113.7",
		},
		{
			name:       "forged X-Forwarded-For is ignored",
			remoteAddr: "203.0.113.7:54321",
			xff:        "1.2.3.4",
			xRealIP:    "5.6.7.8",
			want:       "203.0.113.7",
		},
		{
			name:       "bare host (no port) returned as-is",
			remoteAddr: "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			name:       "IPv6 host:port → host",
			remoteAddr: "[2001:db8::1]:443",
			want:       "2001:db8::1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.remoteAddr, Header: http.Header{}}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-IP", tc.xRealIP)
			}
			if got := audit.ExtractClientIP(r); got != tc.want {
				t.Errorf("ExtractClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractClientIP_PrefersResolvedIP: when chi's ClientIPFrom* middleware
// resolved an IP, that IP wins over the TCP peer (behind a trusted proxy the
// peer is the proxy).
func TestExtractClientIP_PrefersResolvedIP(t *testing.T) {
	var got string
	h := chimw.ClientIPFromXFF("10.0.0.0/8")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = audit.ExtractClientIP(r)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.2:443"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "198.51.100.7" {
		t.Fatalf("ExtractClientIP = %q, want 198.51.100.7", got)
	}
}
