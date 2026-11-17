package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/request"
)

// withRecord mounts the server's root on r: each request's record, with the
// route found on r (as api.routeFinder finds it on the built router).
func withRecord(r chi.Router) {
	r.Use(request.Begin(func(req *http.Request) request.Route {
		return request.Route{Method: req.Method, Pattern: r.Find(chi.NewRouteContext(), req.Method, req.URL.Path)}
	}))
}

// byPattern is the route the tests give the detector: the record's pattern,
// exempt when listed as "METHOD pattern". In the server, api.auditRoute
// gives it.
func byPattern(exempt map[string]bool) func(*http.Request) (string, bool) {
	return func(r *http.Request) (string, bool) {
		pattern := request.Of(r.Context()).Route().Pattern
		return pattern, exempt[r.Method+" "+pattern]
	}
}

// silentBody is what the uncovered route answers, so the test can show the
// detector leaves the answer alone.
const silentBody = `{"id":"hc_inv_1","invoice_number":"INV-0001"}`

// detectorRouter is shaped like the real router: the record and the detector
// at the root, the routes in a mounted subtree. A route whose real audit row
// marks the request cannot be faked here, because only package audit can set
// that mark; internal/api's TestMain (audit_coverage_e2e_test.go) holds it,
// failing when any route of the end-to-end suite is reported.
func detectorRouter(exempt map[string]bool) chi.Router {
	r := chi.NewRouter()
	withRecord(r)
	r.Use(AuditCoverage(byPattern(exempt)))

	v1 := chi.NewRouter()
	// A mutation that commits and writes no audit row: what the detector finds.
	v1.Post("/silent", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom", "kept")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, silentBody)
	})
	v1.Delete("/silent-204/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	v1.Post("/usage-events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	v1.Post("/failing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	v1.Post("/noop", func(w http.ResponseWriter, r *http.Request) {
		audit.MarkSkip(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	v1.Get("/customers", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Mount("/v1", v1)
	return r
}

// The detector counts a successful mutation that wrote no audit row and is
// not exempt, by route pattern, and never changes the answer: the reported
// route's status, body and headers come back as its handler wrote them.
func TestAuditCoverage_ReportsOnlyUncoveredMutations(t *testing.T) {
	exempt := map[string]bool{"POST /v1/usage-events": true}
	for _, tc := range []struct {
		name, method, path string
		route              string // the counter's label: the pattern, never the raw path
		wantStatus         int
		wantReport         bool
	}{
		{"a mutating 2xx with no row and no exemption is reported", "POST", "/v1/silent", "/v1/silent", http.StatusCreated, true},
		{"204 counts as 2xx", "DELETE", "/v1/silent-204/hc_cus_123", "/v1/silent-204/{id}", http.StatusNoContent, true},
		{"an exempt route is not reported", "POST", "/v1/usage-events", "/v1/usage-events", http.StatusAccepted, false},
		{"a failed mutation is not reported", "POST", "/v1/failing", "/v1/failing", http.StatusInternalServerError, false},
		{"a handler that declared it changed nothing (MarkSkip) is not reported", "POST", "/v1/noop", "/v1/noop", http.StatusOK, false},
		{"a GET is not a mutation", "GET", "/v1/customers", "/v1/customers", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := auditUncoveredMutations.WithLabelValues(tc.route)
			before := promtest.ToFloat64(counter)
			rec := httptest.NewRecorder()
			detectorRouter(exempt).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
			want := 0.0
			if tc.wantReport {
				want = 1
			}
			if delta := promtest.ToFloat64(counter) - before; delta != want {
				t.Errorf("hardcount_audit_uncovered_mutation_total{route=%q}: got +%v, want +%v", tc.route, delta, want)
			}
			if tc.path == "/v1/silent" {
				if rec.Body.String() != silentBody || rec.Header().Get("X-Custom") != "kept" || rec.Header().Get("Content-Type") != "application/json" {
					t.Errorf("the reported answer changed: body %q, X-Custom %q, Content-Type %q",
						rec.Body.String(), rec.Header().Get("X-Custom"), rec.Header().Get("Content-Type"))
				}
			}
		})
	}
}
