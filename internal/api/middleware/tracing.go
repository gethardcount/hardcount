package middleware

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// Tracing returns middleware that creates an OpenTelemetry span for each HTTP
// request. It propagates trace context from incoming headers (W3C Trace
// Context). When tracing is off (the noop provider), it adds negligible
// overhead.
//
// The span starts before routing, so it is named by method alone;
// TraceRequestAttrs renames it once the route is known.
func Tracing() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "hardcount-http",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return methodLabel(r.Method)
			}),
		)
	}
}

// TraceRequestAttrs corrects the request span before it ends. otelhttp sets
// url.path to the raw path, which on the public pages carries a link
// token, and client.address to the left-most X-Forwarded-For entry, which
// any client can write. After the handler returns, this sets:
//
//   - the span name to "{method} {route}" (method alone when nothing routed),
//   - http.route to the route pattern on the request's record,
//   - url.path to path(r). path(r) must return a path that holds no link
//     token.
//   - client.address to the record's client address, the trusted one.
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
					semconv.ClientAddress(request.Of(r.Context()).ClientIP()),
				)
				if pat := request.Of(r.Context()).Route().Pattern; pat != "" {
					name += " " + pat
					span.SetAttributes(semconv.HTTPRoute(pat))
				}
				span.SetName(name)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
