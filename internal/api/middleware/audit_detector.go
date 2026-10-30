package middleware

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"

	"github.com/gethardcount/hardcount/internal/audit"
)

// auditUncoveredMutations counts mutating requests that succeeded (2xx) but left
// NO audit evidence and are not declared exempt. Labeled by ROUTE PATTERN — never
// the raw path, which carries ids (and, on the public surfaces, live payment
// tokens) and would blow up cardinality.
//
// A non-zero value means a state change committed with no compliance record: the
// route is missing its audit emission, or a new route shipped without one. Alert
// on any sustained rate. (The route-audit registry's arch test is the BUILD-time
// gate; this is the runtime backstop for the case the static gate cannot see —
// a declared-explicit route whose emission silently stopped firing.)
var auditUncoveredMutations = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "hardcount_audit_uncovered_mutation_total",
		Help: "Successful mutating requests that produced no audit row and are not declared exempt, by route pattern. Any value > 0 is a compliance gap.",
	},
	[]string{"route"},
)

// AuditCoverage returns a middleware that DETECTS uncovered mutations. It is a
// PURE OBSERVER.
//
// It replaces the AuditLog catch-all, which "guaranteed" coverage by writing a
// row for any mutating /v1 request no handler had claimed — inferring the action
// and resource from the URL and sniffing the response body for a label. That
// invented false permanent records in an append-only compliance log (a pricing
// rule deleted from a meter was recorded as "deleted meter X"), and it covered
// only the /v1 block, so /v1/auth, /v1/tenants (since removed), /v1/public/*,
// /v1/webhooks and /v1/bootstrap got nothing at all (ADR-090, RC1 + RC2). Coverage is now
// DECLARED (the route-audit registry) and EMITTED in the business transaction
// (audit.Logger.LogInTx). This middleware only reports when reality diverges.
//
// Two invariants, both load-bearing:
//
//  1. It NEVER MUTATES THE RESPONSE. No status swap, no body rewrite, no error
//     injection. The catch-all's response-swapping ancestor (the per-tenant
//     fail-closed 503) is what caused the idempotency-poisoning bug in ADR-089:
//     the business tx had ALREADY committed, so the 503 was a lie — and the
//     Idempotency layer cached that lie for 24h, stranding the real response and
//     inviting a fresh-key double-mutation. Telemetry does not get to overwrite a
//     committed money mutation's answer. Ever.
//
//  2. It NEVER BUFFERS THE BODY. chi's WrapResponseWriter captures the status
//     with a pass-through Write (only Tee() buffers — we never call it) and
//     preserves http.Flusher / http.Hijacker / io.ReaderFrom through typed
//     wrappers. The catch-all buffered every response to sniff a label out of it,
//     and its buffer implemented none of those interfaces — which is why the CSV
//     exports, on a route given a 5-minute timeout precisely BECAUSE it streams,
//     silently accumulated in memory instead of streaming.
//
// isExempt resolves (method, canonical route pattern) against the registry; it is
// injected because the registry lives in package api (which imports this package).
func AuditCoverage(isExempt func(method, routePattern string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Seed the request-scoped audit bookkeeping for the WHOLE tree. This
			// middleware mounts at the ROOT, so — unlike the /v1-only catch-all it
			// replaced — every emission path gets its client IP and its
			// accounted-for cell, including the public/auth/tenant/webhook blocks.
			// WithRequestState is idempotent by contract: a middleware below this
			// one that re-seeds would otherwise SHADOW this cell, and every covered
			// route would read as uncovered.
			ctx := audit.WithRequestState(r.Context())
			ctx = audit.WithClientIP(ctx, audit.ExtractClientIP(r))
			r = r.WithContext(ctx)

			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			if !isMutating(r.Method) {
				return
			}
			// Only successful mutations are evidence-bearing. Any 2xx counts —
			// several routes answer 204 (deletes) or 202 (async advance), and the
			// billing run answers 206 on a partial cycle.
			if status := ww.Status(); status < 200 || status >= 300 {
				return
			}
			// The request produced an audit row (Log / LogInTx self-mark), or a
			// handler declared it mutated nothing (MarkSkip).
			if audit.WasHandled(r.Context()) {
				return
			}

			// Read the matched pattern SYNCHRONOUSLY, in this frame: mounted
			// subrouters share the root's *chi.Context, which the root returns to a
			// pool once we return — reading it from a goroutine that outlives the
			// request is a use-after-free. An unmatched path (404 inside a mounted
			// subtree) yields e.g. "/v1/customers/*", which deliberately matches no
			// registry key.
			route := "unmatched"
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if pat := rctx.RoutePattern(); pat != "" {
					route = pat
				}
			}

			if isExempt(r.Method, route) {
				return
			}

			auditUncoveredMutations.WithLabelValues(route).Inc()
			slog.ErrorContext(r.Context(), "UNCOVERED MUTATION — a state change committed with no audit row",
				"method", r.Method,
				"route", route,
				"status", ww.Status(),
				"request_id", chimw.GetReqID(r.Context()),
				"remedy", "emit the row in the business transaction (audit.Logger.LogInTx) and declare the route in internal/api/audit_routes.go")
		})
	}
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// AuditUncoveredMutationsByRoute snapshots the uncovered-mutation counter, keyed
// by route pattern.
//
// It exists so a sibling package's end-to-end suite can assert ZERO uncovered
// mutations across every route it drives (internal/api's TestMain) — the runtime
// half of ADR-090's coverage model, complementing the registry's build-time
// two-way diff. Operators read this signal from Prometheus; this accessor is the
// in-process equivalent, and it is deliberately narrow: it collects THIS counter
// only. Gathering the default registry instead would execute every registered
// collector, including the queue-depth GaugeFuncs, which open a database
// transaction per scrape.
func AuditUncoveredMutationsByRoute() map[string]float64 {
	ch := make(chan prometheus.Metric)
	go func() {
		auditUncoveredMutations.Collect(ch)
		close(ch)
	}()

	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "route" {
				out[l.GetValue()] = pb.GetCounter().GetValue()
			}
		}
	}
	return out
}
