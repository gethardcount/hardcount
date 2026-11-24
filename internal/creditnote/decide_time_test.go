package creditnote

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// A create with auto_issue runs on the request's instant and reads no clock
// of its own: the create and the issue share that instant, so the note is
// created and issued at the same time.
func TestCreateRoute_CreateAndIssueShareTheRequestsInstant(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_1": {ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized, Currency: "USD", TotalAmountCents: 10000, AmountDueCents: 10000},
	}}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	server := &cnSteppingClock{at: past.Add(72 * time.Hour)}
	svc.SetClock(server)
	body := `{"invoice_id":"inv_1","reason":"Billing error","auto_issue":true,"lines":[{"description":"x","quantity":1,"unit_amount_cents":1000}]}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	ctx := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), past)
	req = req.WithContext(auth.WithTenantID(ctx, "t1"))
	rec := httptest.NewRecorder()
	NewHandler(svc, HandlerDeps{}).create(rec, req)
	if rec.Code != 201 {
		t.Fatalf("%d %s; want 201", rec.Code, rec.Body.String())
	}
	var cn domain.CreditNote
	if err := json.Unmarshal(rec.Body.Bytes(), &cn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if server.reads != 0 || !cn.CreatedAt.Equal(past) || cn.IssuedAt == nil || !cn.IssuedAt.Equal(past) {
		t.Errorf("%d server reads; created %v, issued %v; want none, both at %v",
			server.reads, cn.CreatedAt, cn.IssuedAt, past)
	}
}
