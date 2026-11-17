package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/platform/request"
)

// accessLines collects the attributes of every "request" line written while
// it is the default logger. A server writes them from its own goroutines.
type accessLines struct {
	mu    sync.Mutex
	lines []map[string]string
}

func (c *accessLines) Enabled(context.Context, slog.Level) bool { return true }
func (c *accessLines) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "request" {
		return nil
	}
	line := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		line[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
	return nil
}
func (c *accessLines) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *accessLines) WithGroup(string) slog.Handler      { return c }

// byRequestID returns the one line of the request with this id.
func (c *accessLines) byRequestID(t *testing.T, id string) map[string]string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range c.lines {
		if line["request_id"] == id {
			return line
		}
	}
	t.Fatalf("no access log line for request %q among %d lines", id, len(c.lines))
	return nil
}

// captureAccessLog makes the default logger collect access log lines until
// the test ends.
func captureAccessLog(t *testing.T) *accessLines {
	t.Helper()
	lines := &accessLines{}
	prev := slog.Default()
	slog.SetDefault(slog.New(lines))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return lines
}

// serveThroughLogger runs one POST /v1/invoices from 203.0.113.9 through
// accessLog and returns its line. inside stands for the layers the logger
// wraps: like authentication, it gets a context made from the logger's.
func serveThroughLogger(t *testing.T, inside func(context.Context)) map[string]string {
	t.Helper()
	lines := captureAccessLog(t)
	type insideKey struct{}
	h := request.Begin(func(r *http.Request) request.Route { return request.Route{Method: r.Method, Pattern: "/v1/invoices"} })(
		accessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inside(context.WithValue(r.Context(), insideKey{}, "added inside the logger"))
			w.WriteHeader(http.StatusOK)
		})))

	req := httptest.NewRequest(http.MethodPost, "/v1/invoices", nil)
	req.RemoteAddr = netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), 4711).String()
	ctx := context.WithValue(req.Context(), chimw.RequestIDKey, "hc_req_abc123")
	// The edge resolves the client address before the logger runs.
	chimw.ClientIPFromRemoteAddr(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(httptest.NewRecorder(), r)
	})).ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	return lines.byRequestID(t, "hc_req_abc123")
}

// A line says who made the request, which tenant it touched and from where,
// carries the request id that joins it to the request's audit rows, and holds
// nothing else: no actor name, which for a user is an email address.
func TestAccessLog_NamesTheCaller(t *testing.T) {
	line := serveThroughLogger(t, func(ctx context.Context) {
		request.SetActor(ctx, request.Actor{Type: "user", ID: "hc_usr_7", TenantID: "hc_ten_42"})
	})
	if line["duration_ms"] == "" {
		t.Error("duration_ms is empty")
	}
	delete(line, "duration_ms")
	want := map[string]string{
		"method":     "POST",
		"path":       "/v1/invoices",
		"status":     "200",
		"request_id": "hc_req_abc123",
		"ip":         "203.0.113.9",
		"actor_type": "user",
		"actor_id":   "hc_usr_7",
		"tenant_id":  "hc_ten_42",
	}
	if !maps.Equal(line, want) {
		t.Errorf("access log line = %v\nwant exactly %v", line, want)
	}
}

// A request nobody signed in for is "anonymous", with no actor id and no
// tenant: an empty field would read as "we lost it".
func TestAccessLog_NobodySignedIn(t *testing.T) {
	line := serveThroughLogger(t, func(context.Context) {})

	if got := line["actor_type"]; got != "anonymous" {
		t.Errorf("actor_type = %q, want anonymous", got)
	}
	for _, key := range []string{"actor_id", "tenant_id"} {
		if got, ok := line[key]; ok {
			t.Errorf("%s = %q, want the field left out", key, got)
		}
	}
	for _, key := range []string{"request_id", "ip"} {
		if line[key] == "" {
			t.Errorf("%s is empty; every line carries it, signed in or not", key)
		}
	}
}

// The production router's access log holds the pattern of the route found at
// the root, even for a request answered before its handler ran, and only the
// first segment of a path that matched no route; a CORS preflight is logged
// too. No line holds the link token.
func TestAccessLog_PathIsAPatternOrTheFirstSegment(t *testing.T) {
	for _, tc := range []struct{ name, method, path, want string }{
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.method == http.MethodOptions {
				req.Header.Set("Origin", "http://localhost:5173")
				req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			}
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			newTestServer().ServeHTTP(httptest.NewRecorder(), req)

			if strings.Contains(logs.String(), fakeLinkToken) {
				t.Errorf("the log holds the link token:\n%s", logs.String())
			}
			got, found := "", false
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var rec struct{ Msg, Path string }
				if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "request" {
					got, found = rec.Path, true
				}
			}
			if !found || got != tc.want {
				t.Errorf("access log path = %q (logged: %v), want %q", got, found, tc.want)
			}
		})
	}
}
