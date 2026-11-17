package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
)

// Every answer carries Hardcount-Request-Id, even one that writes nothing:
// the dashboard falls back to the header when an error body will not parse.
// The id is minted on the server for each request, whatever id the client
// sent.
func TestRequestID_HeaderSetForNonRespondResponses(t *testing.T) {
	const forged = "attacker-chosen-correlation-id"
	seen := map[string]bool{}
	for range 100 {
		req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
		for _, name := range []string{"X-Request-Id", "Request-Id", "X-Correlation-Id"} {
			req.Header.Set(name, forged)
		}
		rec := httptest.NewRecorder()
		mw.RequestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)

		got := rec.Header().Get("Hardcount-Request-Id")
		if !strings.HasPrefix(got, "req_") || len(got) == len("req_") || strings.Contains(got, forged) || seen[got] {
			t.Fatalf("Hardcount-Request-Id = %q, want a new server-minted req_ id", got)
		}
		seen[got] = true
	}
}
