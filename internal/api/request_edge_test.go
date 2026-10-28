package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/gethardcount/hardcount/internal/audit"
)

// A 64-hex capability token, the shape the public invoice and
// payment-update links carry in their path.
const edgeTestToken = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// edgeRouter mounts the production request edge (useRequestEdge) on a chi
// router with one public token route, plus a stand-in for the CORS preflight
// short-circuit that answers OPTIONS before routing. Spans go to a recorder.
func edgeRouter(t *testing.T, trustProxy string) (http.Handler, *tracetest.SpanRecorder, *string) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var seenIP string
	r := chi.NewRouter()
	useRequestEdge(r, trustProxy)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/v1/public/invoices/{token}", func(w http.ResponseWriter, req *http.Request) {
		seenIP = audit.ExtractClientIP(req)
		w.WriteHeader(http.StatusOK)
	})
	return r, rec, &seenIP
}

func TestRequestEdge_ClientIP(t *testing.T) {
	cases := []struct {
		name       string
		trustProxy string
		remoteAddr string
		xff        []string // one entry per header line
		xRealIP    string
		want       string
	}{
		{
			name:       "trusted proxy: right-most untrusted XFF entry",
			trustProxy: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:443",
			xff:        []string{"198.51.100.7, 10.0.0.5"},
			want:       "198.51.100.7",
		},
		{
			name:       "trusted proxy: XFF split over two header lines merges every line",
			trustProxy: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:443",
			xff:        []string{"203.0.113.66", "198.51.100.7"},
			want:       "198.51.100.7",
		},
		{
			name:       "trusted proxy: an entry that is not a bare IP stops the walk at the proxy",
			trustProxy: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:443",
			xff:        []string{"203.0.113.66, 198.51.100.7:4711"},
			want:       "10.0.0.2",
		},
		{
			name:       "trusted proxy: X-Real-IP is not read",
			trustProxy: "10.0.0.0/8",
			remoteAddr: "10.0.0.2:443",
			xRealIP:    "203.0.113.66",
			want:       "10.0.0.2",
		},
		{
			name:       "untrusted peer: forwarding headers are not read",
			trustProxy: "10.0.0.0/8",
			remoteAddr: "203.0.113.9:5555",
			xff:        []string{"198.51.100.7"},
			xRealIP:    "198.51.100.8",
			want:       "203.0.113.9",
		},
		{
			name:       "TRUST_PROXY unset: the TCP peer",
			remoteAddr: "10.0.0.2:443",
			xff:        []string{"198.51.100.7"},
			want:       "10.0.0.2",
		},
		{
			name:       "TRUST_PROXY=none: the TCP peer",
			trustProxy: "none",
			remoteAddr: "203.0.113.9:5555",
			xff:        []string{"198.51.100.7"},
			want:       "203.0.113.9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, seenIP := edgeRouter(t, tc.trustProxy)
			req := httptest.NewRequest(http.MethodGet, "/v1/public/invoices/"+edgeTestToken, nil)
			req.RemoteAddr = tc.remoteAddr
			for _, v := range tc.xff {
				req.Header.Add("X-Forwarded-For", v)
			}
			if tc.xRealIP != "" {
				req.Header.Set("X-Real-IP", tc.xRealIP)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if *seenIP != tc.want {
				t.Fatalf("client IP = %q, want %q", *seenIP, tc.want)
			}
		})
	}
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]string {
	m := map[attribute.Key]string{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value.String()
	}
	return m
}

func TestRequestEdge_SpanCarriesRouteAndTrustedIP_NotToken(t *testing.T) {
	h, rec, _ := edgeRouter(t, "10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/v1/public/invoices/"+edgeTestToken, nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "203.0.113.66, 198.51.100.7")
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if got, want := s.Name(), "GET /v1/public/invoices/{token}"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
	attrs := spanAttrs(s)
	if got, want := attrs["http.route"], "/v1/public/invoices/{token}"; got != want {
		t.Errorf("http.route = %q, want %q", got, want)
	}
	if got, want := attrs["url.path"], "/v1/public/invoices/{token}"; got != want {
		t.Errorf("url.path = %q, want %q", got, want)
	}
	if got, want := attrs["client.address"], "198.51.100.7"; got != want {
		t.Errorf("client.address = %q, want %q (the trusted client IP)", got, want)
	}
	for k, v := range attrs {
		if strings.Contains(v, edgeTestToken) {
			t.Errorf("attribute %s carries the path token: %q", k, v)
		}
	}
}

// A request answered before routing (a CORS preflight) has no route
// pattern; its span must still not carry the token.
func TestRequestEdge_UnroutedSpanHasNoToken(t *testing.T) {
	// Every route that carries a capability token in the path. A CORS
	// preflight is answered before routing, so its span has no route
	// pattern and relies on loggablePath's prefix redaction.
	cases := []struct {
		path string
		want string
	}{
		{"/v1/public/invoices/" + edgeTestToken + "/pdf", "/v1/public/invoices/[redacted]/pdf"},
		{"/v1/public/payment-updates/" + edgeTestToken, "/v1/public/payment-updates/[redacted]"},
		{"/v1/public/cost-dashboard/" + edgeTestToken, "/v1/public/cost-dashboard/[redacted]"},
		{"/v1/auth/invite/" + edgeTestToken, "/v1/auth/invite/[redacted]"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			h, rec, _ := edgeRouter(t, "")
			req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
			req.RemoteAddr = "203.0.113.9:5555"
			req.Header.Set("X-Forwarded-For", "198.51.100.7")
			h.ServeHTTP(httptest.NewRecorder(), req)

			spans := rec.Ended()
			if len(spans) != 1 {
				t.Fatalf("got %d spans, want 1", len(spans))
			}
			s := spans[0]
			if strings.Contains(s.Name(), edgeTestToken) {
				t.Errorf("span name carries the path token: %q", s.Name())
			}
			attrs := spanAttrs(s)
			if got := attrs["url.path"]; got != tc.want {
				t.Errorf("url.path = %q, want %q", got, tc.want)
			}
			if got, want := attrs["client.address"], "203.0.113.9"; got != want {
				t.Errorf("client.address = %q, want %q (the TCP peer; no proxy is trusted)", got, want)
			}
			for k, v := range attrs {
				if strings.Contains(v, edgeTestToken) {
					t.Errorf("attribute %s carries the path token: %q", k, v)
				}
			}
		})
	}
}
