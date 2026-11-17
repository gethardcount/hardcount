package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
)

// Every answer carries Hardcount-Request-Id, even one that writes nothing:
// the dashboard falls back to the header when an error body will not parse.
// The id is minted on the server for each request, whatever id the client
// sent, and the handler's context carries the same id: audit rows are stamped
// from it.
func TestRequestID_HeaderSetForNonRespondResponses(t *testing.T) {
	const forged = "attacker-chosen-correlation-id"
	seen := map[string]bool{}
	for range 100 {
		req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
		for _, name := range []string{"X-Request-Id", "Request-Id", "X-Correlation-Id"} {
			req.Header.Set(name, forged)
		}
		rec := httptest.NewRecorder()
		var inContext string
		mw.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			inContext = chimw.GetReqID(r.Context())
		})).ServeHTTP(rec, req)

		got := rec.Header().Get("Hardcount-Request-Id")
		if !strings.HasPrefix(got, "req_") || len(got) == len("req_") || strings.Contains(got, forged) || seen[got] {
			t.Fatalf("Hardcount-Request-Id = %q, want a new server-minted req_ id", got)
		}
		if inContext != got {
			t.Fatalf("the handler's context carries request id %q, want the header's %q", inContext, got)
		}
		seen[got] = true
	}
}
