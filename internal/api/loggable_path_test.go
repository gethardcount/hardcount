package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// loggablePath returns the matched pattern when there is one, and
// otherwise only the first two segments of the path.
func TestLoggablePath_IsAPatternOrTheFirstTwoSegments(t *testing.T) {
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{"/v1/public/invoices/{token}"}
	req := httptest.NewRequest("GET", "/v1/public/invoices/"+fakeLinkToken, nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	if got, want := loggablePath(req), "/v1/public/invoices/{token}"; got != want {
		t.Errorf("matched route: loggablePath = %q, want %q", got, want)
	}

	for _, tc := range []struct{ path, want string }{
		{"/v1/public/invoices/" + fakeLinkToken, "/v1/public/…"},
		{"/v1/public/invoices/" + fakeLinkToken + "/pdf", "/v1/public/…"},
		{"/V1/public/invoices/" + fakeLinkToken, "/V1/public/…"},
		{"/v1/customers", "/v1/customers"},
		{"/v1/customers/", "/v1/customers/"},
		{"/health", "/health"},
		{"/", "/"},
	} {
		if got := loggablePath(httptest.NewRequest("GET", tc.path, nil)); got != tc.want {
			t.Errorf("no route matched: loggablePath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// P12: a mid-stream export failure must be visible IN the file — the
// status is already 200, so pre-fix the stream just ended and a
// truncated CSV was indistinguishable from a complete one.
func TestExportAbort_WritesIncompleteMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := writeCSVHeaders(rec, httptest.NewRequest("GET", "/v1/exports/customers.csv", nil), "customers")
	_ = cw.Write([]string{"id", "email"})
	_ = cw.Write([]string{"cus_1", "a@b.test"})

	exportAbort(context.Background(), cw, "customers", context.DeadlineExceeded)

	body := rec.Body.String()
	if !strings.Contains(body, "EXPORT_INCOMPLETE") {
		t.Fatalf("aborted export missing the EXPORT_INCOMPLETE marker:\n%s", body)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "EXPORT_INCOMPLETE") {
		t.Errorf("marker is not the final record:\n%s", body)
	}
}
