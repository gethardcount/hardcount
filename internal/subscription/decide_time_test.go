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

// One instant per create: the create runs on the request's instant and
// reads no clock of its own.
func TestCreateRoute_RunsOnTheRequestsInstant(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	srv := &subSteppingClock{at: past.Add(72 * time.Hour)}
	h := NewHandler(NewService(newMemStore(), srv))
	audits := &capturingAudit{}
	h.SetAuditLogger(audits)
	body := `{"code":"sub-time","display_name":"Time","customer_id":"cus_1","items":[{"plan_id":"pln_1"}]}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), past)
	req = req.WithContext(auth.WithTenantID(ctx, "t1"))
	rec := httptest.NewRecorder()
	h.create(rec, req)
	if rec.Code != 201 {
		t.Fatalf("%d %s; want 201", rec.Code, rec.Body.String())
	}
	var created struct {
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !created.StartedAt.Equal(past) {
		t.Errorf("started_at %v (%v); want the request's %v", created.StartedAt, err, past)
	}
	if len(audits.entries) != 1 || srv.reads != 0 {
		t.Errorf("%d audit rows after %d server reads; want 1 after none", len(audits.entries), srv.reads)
	}
}
