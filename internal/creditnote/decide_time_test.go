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

// BindNow binds one read of the service's clock, in either mode, so a
// create, and the issue auto_issue runs after it, read one instant.
func TestBindNow(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, live := range []bool{false, true} {
		svc := NewService(nil, nil, nil)
		srv := &cnSteppingClock{at: past}
		svc.SetClock(srv)
		ctx := svc.BindNow(postgres.WithLivemode(context.Background(), live))
		if at, _ := clock.EffectiveNow(ctx); !at.Equal(past.Add(time.Second)) || srv.reads != 1 {
			t.Errorf("live %v: bound %v after %d server reads; want %v after 1", live, at, srv.reads, past.Add(time.Second))
		}
	}
}

// A create with auto_issue reads the server's clock once: the create and the
// issue share that instant, so the note is created and issued at the same
// time.
func TestCreateRoute_AtRealTimeCreateAndIssueShareOneInstant(t *testing.T) {
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	store := newMemStore()
	invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{
		"inv_1": {ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized, Currency: "USD", TotalAmountCents: 10000, AmountDueCents: 10000},
	}}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	server := &cnSteppingClock{at: past}
	svc.SetClock(server)
	body := `{"invoice_id":"inv_1","reason":"Billing error","auto_issue":true,"lines":[{"description":"x","quantity":1,"unit_amount_cents":1000}]}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(body))
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), "t1"))
	rec := httptest.NewRecorder()
	NewHandler(svc, HandlerDeps{}).create(rec, req)
	if rec.Code != 201 {
		t.Fatalf("%d %s; want 201", rec.Code, rec.Body.String())
	}
	var cn domain.CreditNote
	if err := json.Unmarshal(rec.Body.Bytes(), &cn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := past.Add(time.Second)
	if server.reads != 1 || !cn.CreatedAt.Equal(want) || cn.IssuedAt == nil || !cn.IssuedAt.Equal(want) {
		t.Errorf("%d server reads; created %v, issued %v; want one, both at %v",
			server.reads, cn.CreatedAt, cn.IssuedAt, want)
	}
}
