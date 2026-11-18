package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_http_requests_total",
			Help: "Total HTTP requests by method, path pattern, and status code.",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "hardcount_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
		[]string{"method", "path", "status"},
	)

	httpRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "hardcount_http_requests_in_flight",
			Help: "Current number of HTTP requests being processed.",
		},
	)

	// idempotencyKeyErrors counts idempotency-layer database failures by
	// stage (its metric name keeps "cache" for the alerts and dashboards that
	// match it). None of them runs a handler twice:
	//   - reserve: the key could not be claimed, so the request was refused
	//     with a retryable 503 and the handler did not run.
	//   - finalize: the handler ran but its response was not stored. The key
	//     stays pending, and retries with it get 409.
	//   - release: an answer that frees its key (freesKey) could not free it.
	//     It stays pending, and retries with it get 409 instead of running
	//     again.
	// A sustained rate means clients are being refused or stuck on 409s.
	idempotencyKeyErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "hardcount_idempotency_cache_errors_total",
			Help: "Idempotency-key failures by stage: reserve (the key could not be claimed, on a database error or a client that went away; the request was refused with 503), finalize (response not stored; retries get 409), release (key not freed; retries get 409).",
		},
		[]string{"stage"},
	)
)

// Metrics returns middleware that records HTTP request metrics.
func Metrics() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metrics" || r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}

			httpRequestsInFlight.Inc()
			defer httpRequestsInFlight.Dec()

			start := time.Now()
			next.ServeHTTP(w, r)

			status := strconv.Itoa(request.Of(r.Context()).Status())
			path := routeLabel(r)
			method := methodLabel(r.Method)
			duration := time.Since(start).Seconds()

			httpRequestsTotal.WithLabelValues(method, path, status).Inc()
			httpRequestDuration.WithLabelValues(method, path, status).Observe(duration)
		})
	}
}

// MetricsHandler returns the Prometheus metrics HTTP handler.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

// unmatchedRoute is the path label for any request whose record has no
// route: one that matched no route in the table, an unknown verb, or a CORS
// preflight. It is one constant, so a scanner walking /wp-admin, /.env, /phpmyadmin and so on
// creates one series, not one per probe. audit_detector.go uses the same word
// for the same case.
const unmatchedRoute = "unmatched"

// routeLabel returns the request's route pattern from its record, such as
// /v1/customers/{id}, or unmatchedRoute when it matched none. The set is
// closed: the route table's patterns and unmatchedRoute.
//
// The label is never the raw path. client_golang never evicts vec children,
// so every distinct label value holds about 2 KB of heap for the life of the
// process. The raw path would give every client-chosen path, such as each
// 404 probe, its own series: memory growth that an attacker controls.
func routeLabel(r *http.Request) string {
	if p := request.Of(r.Context()).Route().Pattern; p != "" {
		return p
	}
	return unmatchedRoute
}

// methodLabel bounds the method label to the verbs chi routes, plus "other".
// The method is text the client chooses, so without this bound it would be a
// second unbounded label.
func methodLabel(m string) string {
	switch m {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut, http.MethodTrace,
		"QUERY":
		return m
	}
	return "other"
}
