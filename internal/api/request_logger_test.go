package api

import (
	"context"
	"log/slog"
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

// serveThroughLogger runs one request through requestLogger and returns its
// line. inside stands for the layers the logger wraps. Like authentication,
// it gets a context made from the logger's, never the logger's own.
func serveThroughLogger(t *testing.T, inside func(context.Context)) map[string]string {
	t.Helper()
	lines := captureAccessLog(t)
	type insideKey struct{}

	h := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inside(context.WithValue(r.Context(), insideKey{}, "added inside the logger"))
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/invoices", nil)
	ctx := context.WithValue(req.Context(), chimw.RequestIDKey, "hc_req_abc123")
	// The edge resolves the client address before the logger runs
	// (useRequestEdge); chi keeps it under this helper's key.
	ctx = clientIPContext(t, ctx, "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	return lines.byRequestID(t, "hc_req_abc123")
}

// clientIPContext returns ctx as chi's client-IP middleware leaves it for a
// request from ip.
func clientIPContext(t *testing.T, ctx context.Context, ip string) context.Context {
	t.Helper()
	var out context.Context
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.RemoteAddr = netip.AddrPortFrom(netip.MustParseAddr(ip), 4711).String()
	chimw.ClientIPFromRemoteAddr(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		out = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), req)
	if out == nil {
		t.Fatal("chi's client-IP middleware did not reach its handler")
	}
	return out
}

// A line says who made the request, which tenant it touched and from where,
// and carries the request id that joins it to the request's audit row.
func TestAccessLog_NamesTheCaller(t *testing.T) {
	line := serveThroughLogger(t, func(ctx context.Context) {
		request.SetCaller(ctx, request.Caller{ActorType: "user", ActorID: "hc_usr_7", TenantID: "hc_ten_42"})
	})

	for key, want := range map[string]string{
		"tenant_id":  "hc_ten_42",
		"actor_id":   "hc_usr_7",
		"actor_type": "user",
		"request_id": "hc_req_abc123",
		"ip":         "203.0.113.9",
		"status":     "200",
		"method":     "POST",
	} {
		if got := line[key]; got != want {
			t.Errorf("access log %s = %q, want %q", key, got, want)
		}
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

// A user's name is an email address, and the log goes to sinks that cannot
// erase data. The line carries the actor's id only.
func TestAccessLog_NeverLogsAnActorName(t *testing.T) {
	line := serveThroughLogger(t, func(ctx context.Context) {
		request.SetCaller(ctx, request.Caller{ActorType: "user", ActorID: "hc_usr_7", TenantID: "hc_ten_1"})
	})
	for k, v := range line {
		if strings.Contains(v, "@") {
			t.Errorf("access log %s = %q looks like an email address", k, v)
		}
	}
	if _, ok := line["actor_name"]; ok {
		t.Error("access log has actor_name; a user's name is an email address")
	}
}
