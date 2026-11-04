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

// auditUncoveredMutations counts mutating requests that succeeded (2xx), left
// no audit evidence and are not declared exempt. Its label is the route
// pattern, never the raw path. The raw path carries ids, and on the public
// routes it carries live payment tokens, so it would make the number of
// series grow without limit.
//
// A value above zero means a state change committed with no compliance
// record. Either the route is missing its audit emission, or a new route
// shipped without one. Alert on any sustained rate. The route-audit
// registry's arch test is the check at build time. This counter is the check
// at run time, for the case the static test cannot see: a route declared
// "explicit" (it writes its own row) whose emission stopped firing without a
// word.
var auditUncoveredMutations = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "hardcount_audit_uncovered_mutation_total",
		Help: "Successful mutating requests that produced no audit row and are not declared exempt, by route pattern. Any value > 0 is a compliance gap.",
	},
	[]string{"route"},
)

// AuditCoverage returns a middleware that detects uncovered mutations:
// mutating requests that succeeded, left no audit evidence (a row or a
// MarkSkip) and are not declared exempt. It only observes.
//
// Coverage is declared in the route-audit registry, and each row is written
// in the business transaction (audit.Logger.LogInTx). This middleware only
// reports when the two disagree. It replaces the AuditLog catch-all, and
// ADR-090 (RC1 and RC2) records why.
//
// Two rules hold, and both matter:
//
//  1. It never changes the response: no status swap, no body rewrite, no
//     injected error. The business transaction has already committed, so a
//     changed answer would be false. A client told that a committed change
//     failed may retry with a fresh key and make the change twice. A layer
//     mounted inside Idempotency would also get the false answer cached for
//     24h, which strands the real response (ADR-089). Telemetry never
//     overwrites the answer of a committed money mutation.
//
//  2. It never buffers the body. chi's WrapResponseWriter records the status
//     with a pass-through Write. Only Tee() buffers, and this code never
//     calls it. WrapResponseWriter's typed wrappers keep http.Flusher,
//     http.Hijacker and io.ReaderFrom. A buffer without those interfaces
//     would make a streaming route collect its response in memory instead.
//     The CSV exports are such a route: they get a 5-minute timeout because
//     they stream.
//
// isExempt looks up (method, canonical route pattern) in the registry. It is
// passed in because the registry lives in package api, which imports this
// package.
func AuditCoverage(isExempt func(method, routePattern string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Seed the per-request audit state for the whole route tree. This
			// middleware is mounted at the root, so every emission path gets
			// its client IP and its accounted-for cell, including the public,
			// auth, tenant and webhook blocks. WithRequestState keeps an
			// existing cell, by contract. Otherwise a middleware below this
			// one that seeds again would hide this cell, and every covered
			// route would read as uncovered.
			ctx := audit.WithRequestState(r.Context())
			ctx = audit.WithClientIP(ctx, audit.ExtractClientIP(r))
			r = r.WithContext(ctx)

			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			if !isMutating(r.Method) {
				return
			}
			// Only a successful mutation must leave evidence. Any 2xx counts:
			// several routes answer 204 (deletes) or 202 (async advance), and
			// the billing run answers 206 on a partial cycle.
			if status := ww.Status(); status < 200 || status >= 300 {
				return
			}
			// Covered: the request wrote an audit row (Log and LogInTx mark
			// it themselves), or a handler declared that it changed nothing
			// (MarkSkip).
			if audit.WasHandled(r.Context()) {
				return
			}

			// Read the matched pattern here, synchronously. Mounted
			// subrouters share the root's *chi.Context, and the root returns
			// it to a pool once this handler returns. Reading it from a
			// goroutine that outlives the request is a use-after-free. An
			// unmatched path (a 404 inside a mounted subtree) gives a pattern
			// such as "/v1/customers/*", which matches no registry key on
			// purpose.
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

// AuditUncoveredMutationsByRoute returns a snapshot of the uncovered-mutation
// counter, keyed by route pattern.
//
// A sibling package's end-to-end suite (internal/api's TestMain) uses it to
// assert zero uncovered mutations across every route it drives. That is the
// run-time half of ADR-090's coverage model. The registry's two-way diff at
// build time is the other half. Operators read the same signal from
// Prometheus. This accessor collects only this counter, on purpose. Gathering
// the default registry would run every registered collector, including the
// queue-depth GaugeFuncs, which open a database transaction per scrape.
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
