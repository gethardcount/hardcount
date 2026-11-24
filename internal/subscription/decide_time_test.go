package subscription

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// BindNow binds one read of the service's clock, in either mode, so a create
// and everything it runs read one instant.
func TestBindNow(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, live := range []bool{false, true} {
		srv := &subSteppingClock{at: past}
		svc := NewService(newMemStore(), srv)
		ctx := svc.BindNow(postgres.WithLivemode(context.Background(), live))
		if at, _ := clock.EffectiveNow(ctx); !at.Equal(past.Add(time.Second)) || srv.reads != 1 {
			t.Errorf("live %v: bound %v after %d server reads; want %v after 1", live, at, srv.reads, past.Add(time.Second))
		}
	}
}

// One instant per create: the create and its audit row run on the route's
// one read of the server's clock.
func TestCreateRoute_OneDecisionForTheCreateAndItsAuditRow(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	srv := &subSteppingClock{at: past}
	h := NewHandler(NewService(newMemStore(), srv))
	audits := &capturingAudit{}
	h.SetAuditLogger(audits)
	body := `{"code":"sub-time","display_name":"Time","customer_id":"cus_1","items":[{"plan_id":"pln_1"}]}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), "t1"))
	rec := httptest.NewRecorder()
	h.create(rec, req)
	if rec.Code != 201 {
		t.Fatalf("%d %s; want 201", rec.Code, rec.Body.String())
	}
	var created struct {
		StartedAt time.Time `json:"started_at"`
	}
	want := past.Add(time.Second)
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !created.StartedAt.Equal(want) {
		t.Errorf("started_at %v (%v); want the route's %v", created.StartedAt, err, want)
	}
	if len(audits.entries) != 1 || !audits.entries[0].at.Equal(want) || srv.reads != 1 {
		t.Errorf("audit rows %+v after %d server reads; want one at %v after 1", audits.entries, srv.reads, want)
	}
}
