package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/sagarsuperuser/velox/internal/config"
)

// ParseTrustedProxies parses a comma-separated TRUST_PROXY spec of CIDRs and/or
// bare IPs into networks. A bare IP becomes a /32 (IPv4) or /128 (IPv6).
// Empty input or "none" yields an empty slice, which means "trust no proxy" —
// forwarding headers are then never honored.
//
// It uses config.ParseTrustProxy, the parser behind the boot check, which
// refuses to start on an entry that does not parse. An invalid entry is
// skipped here only for a caller that did not go through config.Load.
func ParseTrustedProxies(spec string) []*net.IPNet {
	nets, _ := config.ParseTrustProxy(spec)
	return nets
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// TrustedRealIP rewrites r.RemoteAddr to the real client IP from
// X-Forwarded-For / X-Real-IP, but ONLY when the immediate TCP peer
// (r.RemoteAddr at entry) is one of the configured trusted proxies. When the
// peer is untrusted — or no proxies are configured — the raw peer address is
// kept, so a client behind no proxy cannot forge a forwarding header to rotate
// its per-IP rate-limit bucket (enumeration bypass) or pin a victim's IP (DoS).
//
// Replaces chi's middleware.RealIP, which trusted those headers unconditionally.
func TrustedRealIP(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := trustedClientIP(r, trusted); ip != "" {
				r.RemoteAddr = ip
			}
			next.ServeHTTP(w, r)
		})
	}
}

// trustedClientIP returns the real client IP from forwarding headers when the
// peer is a trusted proxy, or "" to signal "leave r.RemoteAddr unchanged".
func trustedClientIP(r *http.Request, trusted []*net.IPNet) string {
	peerHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peerHost = r.RemoteAddr
	}
	peerIP := net.ParseIP(strings.TrimSpace(peerHost))
	if peerIP == nil || !ipInAny(peerIP, trusted) {
		// Direct or untrusted peer — forwarding headers are attacker-controlled.
		return ""
	}
	// Peer is a trusted proxy. Walk X-Forwarded-For from the right (closest
	// hop), skipping trusted-proxy hops; the first untrusted address is the
	// real client. This stops a client from injecting fake left-most entries.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			cand := net.ParseIP(strings.TrimSpace(parts[i]))
			if cand == nil || ipInAny(cand, trusted) {
				continue
			}
			return cand.String()
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if cand := net.ParseIP(xr); cand != nil {
			return cand.String()
		}
	}
	return ""
}
