package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// accessLogPath sends req through the production router and returns the
// path its access log line holds. It fails the test if the link token
// appears anywhere in the log.
func accessLogPath(t *testing.T, req *http.Request) string {
	t.Helper()
	srv := newTestServer()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logs.String(), fakeLinkToken) {
		t.Errorf("the log holds the link token:\n%s", logs.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec struct{ Msg, Path string }
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "request" {
			return rec.Path
		}
	}
	t.Fatalf("no access log line:\n%s", logs.String())
	return ""
}

// The access log of the production router holds the pattern of the route
// found at the root, even for a request answered before its handler ran,
// and only the first segment of a path that matched no route. A CORS
// preflight is logged too: the log sits above CORS.
func TestAccessLog_PathIsAPatternOrTheFirstSegment(t *testing.T) {
	cases := []struct {
		name, method, path, want string
	}{
		{"a route refused by authentication logs its pattern", http.MethodGet, "/v1/customers/cus_1", "/v1/customers/{id}"},
		{"a link-token route logs its pattern, never the token", http.MethodGet, "/v1/public/invoices/" + fakeLinkToken, "/v1/public/invoices/{token}"},
		{"the hosted invoice page's address keeps its first segment", http.MethodGet, "/invoice/" + fakeLinkToken, "/invoice/…"},
		{"a path outside every mount keeps one segment, upper case", http.MethodGet, "/V1/public/invoices/" + fakeLinkToken, "/V1/…"},
		{"a path outside every mount keeps one segment, another version", http.MethodGet, "/v2/public/invoices/" + fakeLinkToken, "/v2/…"},
		{"a one-segment path outside every mount is logged whole", http.MethodGet, "/healthz", "/healthz"},
		{"a mistyped path under v1 keeps one segment", http.MethodGet, "/v1/custmers/" + fakeLinkToken, "/v1/…"},
		{"a miss under a link-token mount keeps one segment", http.MethodGet, "/v1/public/invoices/" + fakeLinkToken + "/no-such-page", "/v1/…"},
		{"a mount without the method keeps one segment", http.MethodDelete, "/v1/customers", "/v1/…"},
		{"a CORS preflight is logged", http.MethodOptions, "/v1/public/invoices/" + fakeLinkToken, "/v1/…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.method == http.MethodOptions {
				req.Header.Set("Origin", "http://localhost:5173")
				req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			}
			got := accessLogPath(t, req)
			if got != tc.want {
				t.Errorf("access log path = %q, want %q", got, tc.want)
			}
		})
	}
}

// A CORS preflight's span's url.path keeps only the first segment.
func TestRequestEdge_PreflightSpanKeepsTheFirstSegment(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	srv := newTestServer()

	req := httptest.NewRequest(http.MethodOptions, "/v1/public/invoices/"+fakeLinkToken, nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	srv.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	attrs := spanAttrs(spans[0])
	if got, want := attrs["url.path"], "/v1/…"; got != want {
		t.Errorf("url.path = %q, want %q", got, want)
	}
	for k, v := range attrs {
		if strings.Contains(v, fakeLinkToken) {
			t.Errorf("attribute %s holds the link token: %q", k, v)
		}
	}
}
