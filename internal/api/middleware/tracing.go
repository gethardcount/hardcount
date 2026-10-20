package middleware

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/sagarsuperuser/velox/internal/audit"
)

// Tracing returns middleware that creates OpenTelemetry spans for each HTTP request.
// Propagates trace context from incoming headers (W3C Trace Context).
// When tracing is disabled (noop provider), this adds negligible overhead.
//
// The span starts before routing, so it is named by method alone;
// TraceRequestAttrs renames it once the route is known.
func Tracing() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "velox-http",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return methodLabel(r.Method)
			}),
		)
	}
}

// TraceRequestAttrs corrects the request span before it ends. otelhttp sets
// url.path to the raw path, which on the public pages carries a capability
// token, and client.address to the left-most X-Forwarded-For entry, which
// any client can write. After the handler returns, this sets:
//
//   - the span name to "{method} {route}" (method alone when nothing routed),
//   - http.route to chi's route pattern,
//   - url.path to path(r), which must leave no token in the path,
//   - client.address to audit.ExtractClientIP, the trusted client IP.
//
// Mount it after Tracing and TrustedClientIP and before anything that can
// answer without routing (CORS preflight), so every span passes through it.
func TraceRequestAttrs(path func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				span := trace.SpanFromContext(r.Context())
				if !span.IsRecording() {
					return
				}
				name := methodLabel(r.Method)
				span.SetAttributes(
					semconv.URLPath(path(r)),
					semconv.ClientAddress(audit.ExtractClientIP(r)),
				)
				if rctx := chi.RouteContext(r.Context()); rctx != nil {
					if pat := rctx.RoutePattern(); pat != "" {
						name += " " + pat
						span.SetAttributes(semconv.HTTPRoute(pat))
					}
				}
				span.SetName(name)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
