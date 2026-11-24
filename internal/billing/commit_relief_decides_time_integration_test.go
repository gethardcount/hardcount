package billing_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// reliefThroughTheRoute pays a commit invoice and sends its commit relief
// through the credit note route on real stores, with at bound on the request
// as the API binds a request's instant, and returns the note.
func reliefThroughTheRoute(t *testing.T, name string, at time.Time) domain.CreditNote {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	settings := tenant.NewSettingsStore(db)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + tenantID, DisplayName: name})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	creditSvc := credit.NewService(credit.NewPostgresStore(db))
	invStore := invoice.NewPostgresStore(db)
	invStore.SetCommitFunder(creditSvc)
	invSvc := invoice.NewService(invStore, clock.Real(), settings)
	draft := createCommitInvoice(t, ctx, invSvc, tenantID, cust.ID, 10000, 10000)
	if _, err := invSvc.Finalize(ctx, tenantID, draft.ID); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := invStore.MarkPaidOffline(ctx, tenantID, draft.ID, time.Now().UTC(), nil); err != nil {
		t.Fatalf("pay: %v", err)
	}
	svc := newReliefService(db, invStore, creditSvc, settings)
	body := `{"invoice_id":"` + draft.ID + `","reason":"Lost card dispute","commit_relief":{"retire_all":true}}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req = req.WithContext(clock.WithEffectiveNow(auth.WithTenantID(ctx, tenantID), at))
	rec := httptest.NewRecorder()
	creditnote.NewHandler(svc, creditnote.HandlerDeps{}).Routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("relief: %d %s; want 201", rec.Code, rec.Body.String())
	}
	var cn domain.CreditNote
	if err := json.Unmarshal(rec.Body.Bytes(), &cn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cn
}

// On the real stores, through the credit note route: the relief runs on the
// request's instant (here a fixed past instant, not the machine's), and its
// note is created and issued at it.
func TestCommitRelief_IsAtTheRequestsInstant(t *testing.T) {
	at := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	cn := reliefThroughTheRoute(t, "Commit Relief Request Instant", at)
	if !cn.CreatedAt.Equal(at) || cn.IssuedAt == nil || !cn.IssuedAt.Equal(at) {
		t.Errorf("created_at %v, issued_at %v; want both at the request's %v", cn.CreatedAt, cn.IssuedAt, at)
	}
}
