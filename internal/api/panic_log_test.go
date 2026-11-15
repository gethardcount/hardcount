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

// panicLogToken is a capability token of the shape the public links carry.
const panicLogToken = "c0ffee0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// panicRouter mounts the panic middleware on a root router the way
// NewServer does, and the public invoice routes the way NewServer mounts
// them (a Route whose subrouter holds "/{token}"). The token handler and
// the subtree's not-found handler both panic. When panicBeforeRouting is
// set, a root middleware after the panic middleware panics instead, so the
// panic happens before any route has matched. Every log line goes to the
// returned buffer.
func panicRouter(t *testing.T, panicBeforeRouting bool) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

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
	return r, &logs
}

// A panic on a route whose path holds a token logs the route's pattern,
// never the token.
func TestPanicLog_NamesTheRouteNotTheToken(t *testing.T) {
	h, logs := panicRouter(t, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/public/invoices/"+panicLogToken, nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	out := logs.String()
	if !strings.Contains(out, "handler panic") {
		t.Fatalf("no panic line logged:\n%s", out)
	}
	if strings.Contains(out, panicLogToken) {
		t.Errorf("the panic log holds the path token:\n%s", out)
	}
	if !strings.Contains(out, "path=/v1/public/invoices/{token}") {
		t.Errorf("the panic log does not name the route pattern:\n%s", out)
	}
}

// A panic before any route matched, or in a not-found handler, has no full
// route pattern to log; the token must still stay out of the log.
func TestPanicLog_UnmatchedPathHoldsNoToken(t *testing.T) {
	cases := []struct {
		name               string
		panicBeforeRouting bool
		path               string
	}{
		{"before routing, invoice", true, "/v1/public/invoices/" + panicLogToken + "/pdf"},
		{"before routing, payment update", true, "/v1/public/payment-updates/" + panicLogToken},
		{"before routing, cost dashboard", true, "/v1/public/cost-dashboard/" + panicLogToken},
		{"before routing, invite", true, "/v1/auth/invite/" + panicLogToken},
		{"not found under a token route", false, "/v1/public/invoices/" + panicLogToken + "/no-such-page"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, logs := panicRouter(t, tc.panicBeforeRouting)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			out := logs.String()
			if !strings.Contains(out, "handler panic") {
				t.Fatalf("no panic line logged:\n%s", out)
			}
			if strings.Contains(out, panicLogToken) {
				t.Errorf("the panic log holds the path token:\n%s", out)
			}
		})
	}
}
