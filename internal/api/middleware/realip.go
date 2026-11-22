package middleware

import (
	"net"
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// TrustedClientIP resolves the client IP with chi's ClientIPFrom* middleware
// and stores it on the request context, where the request's record takes it
// (request.Record.ClientIP). r.RemoteAddr is left as the TCP peer.
//
//   - With trusted proxies, chi's ClientIPFromXFF reads X-Forwarded-For only
//     when the TCP peer is inside one of them. It merges every header line,
//     walks right to left past trusted hops and takes the first untrusted
//     address. An entry that is not a bare IP ends the walk with no result.
//   - With none, chi's ClientIPFromRemoteAddr uses the TCP peer.
//
// No other header (X-Real-IP, True-Client-IP) is read. When chi resolves
// nothing, the record falls back to the TCP peer.
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
