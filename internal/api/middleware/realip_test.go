package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
)

// TestTrustedClientIP: the client IP comes from chi's ClientIPFrom*
// middleware, read through audit.ExtractClientIP. X-Forwarded-For counts only
// when the TCP peer is a trusted proxy, and r.RemoteAddr is left as the peer.
func TestTrustedClientIP(t *testing.T) {
	trusted := ParseTrustedProxies("10.0.0.0/8,127.0.0.1")

	run := func(remoteAddr, xff, xrealip string, nets []*net.IPNet) (ip, remote string) {
		h := TrustedClientIP(nets)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			ip, remote = audit.ExtractClientIP(r), r.RemoteAddr
		}))
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		if xrealip != "" {
			req.Header.Set("X-Real-IP", xrealip)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		return ip, remote
	}

	cases := []struct {
		name, remoteAddr, xff, xrealip string
		nets                           []*net.IPNet
		want                           string
	}{
		{"untrusted peer: forged XFF ignored", "203.0.113.9:5555", "1.2.3.4", "", trusted, "203.0.113.9"},
		{"trusted peer: XFF honored", "10.1.2.3:443", "198.51.100.7", "", trusted, "198.51.100.7"},
		{"trusted peer: first untrusted from the right", "127.0.0.1:8080", "198.51.100.7, 10.0.0.5, 10.0.0.6", "", trusted, "198.51.100.7"},
		{"trusted peer: X-Real-IP not read", "10.0.0.9:80", "", "192.0.2.44", trusted, "10.0.0.9"},
		{"trusted peer, every XFF hop trusted: the peer", "10.0.0.9:80", "10.0.0.5", "", trusted, "10.0.0.9"},
		{"IPv4-mapped trusted peer still matches", "[::ffff:10.1.2.3]:443", "198.51.100.7", "", trusted, "198.51.100.7"},
		{"no trusted proxies configured: the peer", "10.1.2.3:443", "198.51.100.7", "", nil, "10.1.2.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, remote := run(tc.remoteAddr, tc.xff, tc.xrealip, tc.nets)
			if ip != tc.want {
				t.Errorf("client IP = %q, want %q", ip, tc.want)
			}
			if remote != tc.remoteAddr {
				t.Errorf("r.RemoteAddr = %q, want it left as the peer %q", remote, tc.remoteAddr)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	nets := ParseTrustedProxies("10.0.0.0/8, 127.0.0.1 , , bogus")
	if len(nets) != 2 {
		t.Fatalf("parsed %d nets, want 2 (CIDR + bare IP; blank/bogus skipped)", len(nets))
	}
}

func TestParseTrustedProxies_NoneMeansNoProxy(t *testing.T) {
	for _, spec := range []string{"none", " NONE ", "None"} {
		if got := ParseTrustedProxies(spec); got != nil {
			t.Fatalf("ParseTrustedProxies(%q) = %v, want nil", spec, got)
		}
	}
	if got := ParseTrustedProxies("10.0.0.0/8"); len(got) != 1 {
		t.Fatalf("a CIDR must still parse, got %v", got)
	}
}
