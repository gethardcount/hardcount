package middleware

import (
	"context"
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/xid"
)

// requestIDPrefix marks an id as minted by the server. audit_log rows written
// before this middleware existed may carry a request_id the client chose, and
// audit_log is append-only, so they cannot be rewritten. The prefix also
// tells a forensic reader which era a row belongs to: a `req_` id is provably
// ours.
const requestIDPrefix = "req_"

// RequestID replaces chi's middleware.RequestID. It always mints the id on
// the server and never honours an inbound header.
//
// chi's version does this (chi/v5 middleware/request_id.go):
//
//	requestID := r.Header.Get("X-Request-Id")   // <- client-controlled
//	if requestID == "" { requestID = <generated> }
//
// With chi's version, any caller could choose the request_id on their own
// audit_log rows. The audit UI presents that column as forensic evidence for
// correlation, and support uses it to join a customer's report to the server
// logs. An attacker could set it to a constant so their actions cannot be
// joined, collide it with an innocent tenant's traffic, or forge a value that
// "proves" an action came from somewhere it did not. Correlation evidence that
// an adversary can write is not evidence. CloudTrail's eventID, Stripe's
// request id and GCP's insertId are all minted on the server for this reason.
//
// The inbound value is dropped entirely, not kept under a second key for log
// correlation, for these reasons:
//
//   - Nothing consumes it as input. The name still appears in tests that
//     forge the header to prove it is ignored, and in the docs that record
//     this decision. Hardcount's published contract is the
//     Hardcount-Request-Id response header (respond.go). The dashboard
//     captures it (web-v2 lib/api.ts), and the docs point support at it.
//     That contract is unchanged.
//   - Cross-service trace continuity belongs to W3C Trace Context, which
//     mw.Tracing() (otelhttp) propagates from inbound headers. Tracing is a
//     no-op unless OTEL_EXPORTER_OTLP_ENDPOINT is set
//     (internal/platform/telemetry), so a default deployment has no
//     cross-service correlation channel at all. That is a real, if small,
//     accepted loss. A caller that wants to match its own request to a
//     Hardcount audit row must read the Hardcount-Request-Id response header
//     instead of choosing the id up front. The trade is worth it, because an
//     inbound X-Request-Id would be a second, weaker, unauthenticated
//     channel, and it is the one that lands in the compliance log. If a
//     caller has a real need for cross-service correlation, wire OTLP, not a
//     client-chosen id.
//   - Recording the client's string anywhere on the row, even under an
//     honestly named metadata key, puts unverified client input into a
//     permanent append-only compliance record. The audit redesign (ADR-090)
//     lets nothing unverified enter that log, and a key nobody reads is not
//     worth the exception.
//
// The id is stored under chi's own RequestIDKey. So chimw.GetReqID(ctx), the
// accessor that audit.Logger, telemetry.ContextHandler, respond.go and
// payment/stripe already call, keeps working, and a request id is born in
// exactly one place.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDPrefix + xid.New().String()
		ctx := context.WithValue(r.Context(), chimw.RequestIDKey, id)
		// Set the published header here, not only in respond.JSON. Otherwise
		// a response that never reaches a respond helper has no id in the
		// header or the body. Two such responses are chi's default "404 page
		// not found" for an unrouted path and net/http's "400 Bad Request"
		// for a malformed request line. With no envelope to parse, the
		// caller needs the header most. Setting it before next.ServeHTTP
		// covers whoever writes the status. respond.JSON sets the same value
		// again, a no-op.
		w.Header().Set("Hardcount-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
