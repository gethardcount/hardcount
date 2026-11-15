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

// The access log of the production router holds the route's pattern when
// a route or a mount matched, and only the first two segments of a path
// that matched nothing.
func TestAccessLog_PathIsAPatternOrTheFirstTwoSegments(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"a path outside every mount keeps two segments, upper case", "/V1/public/invoices/" + fakeLinkToken, "/V1/public/…"},
		{"a path outside every mount keeps two segments, another version", "/v2/public/invoices/" + fakeLinkToken, "/v2/public/…"},
		{"a short path outside every mount is logged whole", "/v2/" + "nothing", "/v2/nothing"},
		{"a mistyped path under v1 logs the mount pattern", "/v1/custmers/" + fakeLinkToken, "/v1/*"},
		{"a miss under a link-token mount logs the mount pattern", "/v1/public/invoices/" + fakeLinkToken + "/no-such-page", "/v1/public/invoices/*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := accessLogPath(t, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if got != tc.want {
				t.Errorf("access log path = %q, want %q", got, tc.want)
			}
		})
	}
}

// A CORS preflight is answered before routing, so it never reaches the
// access log; its span's url.path keeps only the first two segments.
func TestRequestEdge_PreflightSpanKeepsTwoSegments(t *testing.T) {
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
	if got, want := attrs["url.path"], "/v1/public/…"; got != want {
		t.Errorf("url.path = %q, want %q", got, want)
	}
	for k, v := range attrs {
		if strings.Contains(v, fakeLinkToken) {
			t.Errorf("attribute %s holds the link token: %q", k, v)
		}
	}
}
