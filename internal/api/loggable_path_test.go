package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// P12: the request logger wrote raw URL paths — the public surfaces
// carry bearer-equivalent capability tokens IN the path, so every
// hosted-invoice view and payment-update click deposited a live
// payment credential into the log sink.
//
// Mutation-verify: return r.URL.Path directly from loggablePath — the
// token-absence assertions fail.
func TestLoggablePath_NeverContainsTokens(t *testing.T) {
	const token = "tok_2f8a9bc31c4d5e6f708192a3b4c5d6e7"

	// Matched route: chi's pattern replaces the token segment.
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{"/v1/public/invoices/{token}"}
	req := httptest.NewRequest("GET", "/v1/public/invoices/"+token, nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	if got := loggablePath(req); strings.Contains(got, token) {
		t.Errorf("matched-route path leaks the token: %q", got)
	}

	// Unmatched probe (no chi pattern, e.g. a 404 with a candidate
	// token): known public prefixes are redacted.
	for _, path := range []string{
		"/v1/public/invoices/" + token,
		"/v1/public/invoices/" + token + "/pdf",
		"/v1/public/payment-updates/" + token,
		"/v1/public/cost-dashboard/" + token,
		"/v1/auth/invite/" + token,
	} {
		req := httptest.NewRequest("GET", path, nil)
		got := loggablePath(req)
		if strings.Contains(got, token) {
			t.Errorf("unmatched path leaks the token: %q -> %q", path, got)
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("unmatched public path not redacted: %q -> %q", path, got)
		}
	}

	// Sub-path after the token survives (operational value, no secret).
	req = httptest.NewRequest("GET", "/v1/public/invoices/"+token+"/pdf", nil)
	if got := loggablePath(req); !strings.HasSuffix(got, "/pdf") {
		t.Errorf("sub-path lost in redaction: %q", got)
	}

	// Ordinary paths pass through untouched.
	req = httptest.NewRequest("GET", "/v1/customers", nil)
	if got := loggablePath(req); got != "/v1/customers" {
		t.Errorf("ordinary path altered: %q", got)
	}
}

// P12: a mid-stream export failure must be visible IN the file — the
// status is already 200, so pre-fix the stream just ended and a
// truncated CSV was indistinguishable from a complete one.
func TestExportAbort_WritesIncompleteMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := writeCSVHeaders(rec, "customers")
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
