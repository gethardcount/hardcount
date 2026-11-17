package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
)

// loggablePath returns the matched route's pattern when there is one
// (TestAccessLog_PathIsAPatternOrTheFirstSegment), and otherwise only the first
// segment of the path.
func TestLoggablePath_IsAPatternOrTheFirstSegment(t *testing.T) {
	logged := func(req *http.Request) string {
		var got string
		requesttest.Root(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = loggablePath(r) })).
			ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	for _, tc := range []struct{ name, path, want string }{
		{"a link under v1 keeps one segment", "/v1/public/invoices/" + fakeLinkToken + "/pdf", "/v1/…"},
		{"the hosted invoice page's address keeps one segment", "/invoice/" + fakeLinkToken, "/invoice/…"},
		{"an upper-case version keeps one segment", "/V1/public/invoices/" + fakeLinkToken, "/V1/…"},
		{"a path starting with two slashes keeps its empty first segment", "//a/b/c", "//…"},
		{"an encoded slash is cut after it decodes", "/a/b%2Fc/d", "/a/…"},
		{"a two-segment path keeps one segment", "/v1/customers", "/v1/…"},
		{"a one-segment path is logged whole", "/a", "/a"},
		{"a one-segment path with a trailing slash is logged whole", "/a/", "/a/"},
		{"the root is logged whole", "/", "/"},
		{"an empty path is logged whole", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tc.path != "" {
				req = httptest.NewRequest("GET", tc.path, nil)
			} else {
				req.URL.Path = ""
			}
			if got := logged(req); got != tc.want {
				t.Errorf("loggablePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
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
