package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
)

// fakeLinkToken stands in for the link token a public link carries in its
// path. It is plainly fake so a failure that prints it is easy to read.
const fakeLinkToken = "FAKE-LINK-TOKEN-DO-NOT-LOG"

// captureLogs sends every log line to the returned buffer until the test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// panicLine returns the "handler panic" line cut before its stack, so the
// line ends with the path field. It fails the test when no such line was
// logged.
func panicLine(t *testing.T, logs *bytes.Buffer) string {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="handler panic"`) {
			if i := strings.Index(line, " stack="); i >= 0 {
				return line[:i]
			}
			return line
		}
	}
	t.Fatalf("no panic line logged:\n%s", logs.String())
	return ""
}

// panickingWriter is a ResponseWriter whose first status write panics, so
// any answer the router gives panics inside the panic middleware.
type panickingWriter struct{ *httptest.ResponseRecorder }

func (panickingWriter) WriteHeader(int) { panic("writing the status") }

// The production router logs a panic on a link-token route with the
// route's pattern, never the token. The 404 under the hosted invoice mount
// is the answer that panics: it is the one a router built on an empty
// database gives on that path without touching the database.
func TestPanicLog_RealRouterLogsThePatternNotTheLinkToken(t *testing.T) {
	srv := newTestServer()
	logs := captureLogs(t)
	w := panickingWriter{httptest.NewRecorder()}
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/public/invoices/"+fakeLinkToken+"/no-such-page", nil))

	line := panicLine(t, logs)
	if strings.Contains(logs.String(), fakeLinkToken) {
		t.Errorf("the log holds the link token:\n%s", line)
	}
	if !strings.HasSuffix(line, " path=/v1/public/invoices/*") {
		t.Errorf("the panic line does not hold the mount's pattern:\n%s", line)
	}
}

// panicRouter is a copy of the chain around the panic middleware: a root
// router with Recoverer given loggablePath, and the hosted invoice routes
// as a Route whose subrouter holds "/{token}". Its token handler and its
// subtree's not-found handler both panic. With panicBeforeRouting set, a
// root middleware after Recoverer panics instead, before any route has
// matched. The real-router test above covers the production wiring.
func panicRouter(panicBeforeRouting bool) http.Handler {
	r := chi.NewRouter()
	r.Use(mw.Recoverer(loggablePath))
	if panicBeforeRouting {
		r.Use(func(http.Handler) http.Handler {
			return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("before routing") })
		})
	}
	r.Route("/v1/public/invoices", func(r chi.Router) {
		sub := chi.NewRouter()
		sub.Get("/{token}", func(http.ResponseWriter, *http.Request) { panic("in the handler") })
		sub.NotFound(func(http.ResponseWriter, *http.Request) { panic("in not found") })
		r.Mount("/", sub)
	})
	return r
}

// Each case names the path the panic line must hold for a request whose
// path holds a link token.
func TestPanicLog_LogsASafePath(t *testing.T) {
	cases := []struct {
		name               string
		panicBeforeRouting bool
		path               string
		want               string
	}{
		{"a matched route logs its pattern", false,
			"/v1/public/invoices/" + fakeLinkToken, "/v1/public/invoices/{token}"},
		{"a miss under a mount logs the mount's pattern", false,
			"/v1/public/invoices/" + fakeLinkToken + "/no-such-page", "/v1/public/invoices/*"},
		{"before routing an invoice link is redacted", true,
			"/v1/public/invoices/" + fakeLinkToken + "/pdf", "/v1/public/invoices/[redacted]/pdf"},
		{"before routing a card update link is redacted", true,
			"/v1/public/payment-updates/" + fakeLinkToken, "/v1/public/payment-updates/[redacted]"},
		{"before routing a cost dashboard link is redacted", true,
			"/v1/public/cost-dashboard/" + fakeLinkToken, "/v1/public/cost-dashboard/[redacted]"},
		{"before routing an invite link is redacted", true,
			"/v1/auth/invite/" + fakeLinkToken, "/v1/auth/invite/[redacted]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			rec := httptest.NewRecorder()
			panicRouter(tc.panicBeforeRouting).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			line := panicLine(t, logs)
			if strings.Contains(logs.String(), fakeLinkToken) {
				t.Errorf("the log holds the link token:\n%s", line)
			}
			if !strings.HasSuffix(line, " path="+tc.want) {
				t.Errorf("panic line path is not %q:\n%s", tc.want, line)
			}
		})
	}
}
