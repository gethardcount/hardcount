package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// An export that fails mid-stream ends the file with an EXPORT_INCOMPLETE
// record: its status is already 200, so the file is the only place a cut can
// show.
func TestExportAbort_EndsTheFileWithAnIncompleteMarker(t *testing.T) {
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
