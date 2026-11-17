package middleware

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/request"
)

// auditUncoveredMutations counts mutating requests that succeeded (2xx), left
// no audit evidence and are not declared exempt. Its label is the route
// pattern: the raw path carries ids, and payment tokens on the public pages,
// so it would grow the series without limit.
//
// A value above zero is a state change committed with no audit row: alert on
// it. The route table's test checks that every route declares its audit; this
// counter catches at run time what that test cannot, a route declared
// explicit whose own row stopped being written.
var auditUncoveredMutations = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "hardcount_audit_uncovered_mutation_total",
		Help: "Successful mutating requests that produced no audit row and are not declared exempt, by route pattern. Any value > 0 is a compliance gap.",
	},
	[]string{"route"},
)

// AuditCoverage returns the middleware that counts uncovered mutations:
// mutating requests that succeeded, left no audit evidence (a row or a
// MarkSkip) and are not declared exempt. route gives the request's route
// pattern, the counter's label, and whether the route table declares the
// route exempt. It is passed in because the table lives in package api, which
// imports this one.
//
// It only observes. It never changes the answer: the change has committed, so
// another answer would be false, and a client told that a committed change
// failed may make it again (ADR-089). It wraps no writer either: it reads the
// status from the request's record.
func AuditCoverage(route func(*http.Request) (pattern string, exempt bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)

			if !isMutating(r.Method) {
				return
			}
			// Only a successful mutation must leave evidence, and any 2xx is
			// one: 202, 204 and 206 included.
			status := request.Of(r.Context()).Status()
			if status < 200 || status >= 300 {
				return
			}
			// Covered: the request wrote an audit row (Log and LogInTx mark
			// it themselves), or a handler declared that it changed nothing
			// (MarkSkip).
			if audit.WasHandled(r.Context()) {
				return
			}

			pattern, exempt := route(r)
			if exempt {
				return
			}
			if pattern == "" {
				pattern = "unmatched"
			}

			auditUncoveredMutations.WithLabelValues(pattern).Inc()
			slog.ErrorContext(r.Context(), "UNCOVERED MUTATION — a state change committed with no audit row",
				"method", r.Method,
				"path", pattern,
				"status", status,
				"remedy", "emit the row in the business transaction (audit.Logger.LogInTx) and declare the route in internal/api/route_table.go")
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

// AuditUncoveredMutationsByRoute returns the uncovered-mutation counter's
// values by route pattern, so a test can assert that the routes it drove left
// none. It collects only this counter: gathering the default registry would
// run every collector there, such as the server's queue-depth gauges, which
// query the database.
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
