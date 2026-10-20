package middleware

import (
	"net"
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"

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

// TrustedClientIP resolves the client IP with chi's ClientIPFrom* middleware
// and stores it on the request context; read it with audit.ExtractClientIP
// (or chi's GetClientIP). r.RemoteAddr is left as the TCP peer.
//
//   - With trusted proxies, chi's ClientIPFromXFF reads X-Forwarded-For only
//     when the TCP peer is inside one of them. It merges every header line,
//     walks right to left past trusted hops and takes the first untrusted
//     address. An entry that is not a bare IP ends the walk with no result.
//   - With none, chi's ClientIPFromRemoteAddr uses the TCP peer.
//
// No other header (X-Real-IP, True-Client-IP) is read. When chi resolves
// nothing, audit.ExtractClientIP falls back to the TCP peer.
func TrustedClientIP(trusted []*net.IPNet) func(http.Handler) http.Handler {
	if len(trusted) == 0 {
		return chimw.ClientIPFromRemoteAddr
	}
	prefixes := make([]string, len(trusted))
	for i, n := range trusted {
		prefixes[i] = n.String()
	}
	return chimw.ClientIPFromXFF(prefixes...)
}
