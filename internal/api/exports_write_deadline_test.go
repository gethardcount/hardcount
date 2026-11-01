package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

// deadlineRecorder records the write deadline a handler sets, and whether it
// was set before the first byte of the response.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline      time.Time
	beforeAnyByte bool
	wrote         bool
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadline, d.beforeAnyByte = t, !d.wrote
	return nil
}

func (d *deadlineRecorder) WriteHeader(code int) {
	d.wrote = true
	d.ResponseRecorder.WriteHeader(code)
}

func (d *deadlineRecorder) Write(b []byte) (int, error) {
	d.wrote = true
	return d.ResponseRecorder.Write(b)
}

type okExportAuditor struct{}

func (okExportAuditor) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return nil
}

type twoAuditEntries struct{}

func (twoAuditEntries) Stream(_ context.Context, _ string, _ audit.QueryFilter, fn func(domain.AuditEntry) error) error {
	for _, id := range []string{"aud_1", "aud_2"} {
		if err := fn(domain.AuditEntry{ID: id, CreatedAt: time.Now(), Action: "update"}); err != nil {
			return err
		}
	}
	return nil
}

// TestExport_ExtendsItsWriteDeadline: an export moves its response's write
// deadline to exportTimeout + exportWriteGrace, before the first byte. The
// server's WriteTimeout (35 s, cmd/hardcount/main.go) otherwise cut every
// export at 35 s, mid-file, although the /v1/exports routes allow five
// minutes. The writer is wrapped as the middleware wraps it, so the deadline
// is shown to reach the connection through Unwrap.
// Mutation-verify: drop the extendExportWriteDeadline call in
// writeCSVHeaders → no deadline is set and this fails.
func TestExport_ExtendsItsWriteDeadline(t *testing.T) {
	h := &exportsHandler{audit: okExportAuditor{}, auditLog: twoAuditEntries{}}
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/v1/exports/audit-log.csv", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), "hc_ten_test"))

	before := time.Now()
	h.exportAuditLog(chimw.NewWrapResponseWriter(rec, req.ProtoMajor), req)
	after := time.Now()

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "aud_2") {
		t.Fatalf("export answered %d %q, want 200 with both rows", rec.Code, rec.Body.String())
	}
	if rec.deadline.IsZero() {
		t.Fatal("the export set no write deadline: the server's 35 s WriteTimeout still cuts it")
	}
	lo, hi := before.Add(exportTimeout+exportWriteGrace), after.Add(exportTimeout+exportWriteGrace)
	if rec.deadline.Before(lo) || rec.deadline.After(hi) {
		t.Fatalf("write deadline %v from the start, want %v (the export timeout plus its grace)",
			rec.deadline.Sub(before).Round(time.Second), exportTimeout+exportWriteGrace)
	}
	if !rec.beforeAnyByte {
		t.Fatal("the deadline was set after the response began")
	}
}
