package invoice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

type auditCall struct {
	action string
	label  string
	meta   map[string]any
}

// capturingInvoiceAudit records audit Log calls. Satisfies auditWriter.
type capturingInvoiceAudit struct{ calls []auditCall }

func (c *capturingInvoiceAudit) Log(_ context.Context, _, action, _, _, label string, meta map[string]any) error {
	c.calls = append(c.calls, auditCall{action: action, label: label, meta: meta})
	return nil
}

func (c *capturingInvoiceAudit) firstOf(action string) (auditCall, bool) {
	for _, e := range c.calls {
		if e.action == action {
			return e, true
		}
	}
	return auditCall{}, false
}

func (c *capturingInvoiceAudit) actions() []string {
	out := make([]string, 0, len(c.calls))
	for _, e := range c.calls {
		out = append(out, e.action)
	}
	return out
}

// Collecting payment on a finalized invoice must write an explicit "collect"
// audit row (labelled with the invoice number), NOT fall through to the
// middleware catch-all which records POST /collect as a generic "create"
// ("Created INV-NNN") — a money-movement action mislabeled as creation.
func TestCollectPayment_WritesCollectAuditRow(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	inv, err := store.Create(ctx, "t1", domain.Invoice{
		InvoiceNumber:  "INV-001",
		Status:         domain.InvoiceFinalized,
		PaymentStatus:  domain.PaymentPending,
		AmountDueCents: 5000,
		CustomerID:     "cus_1",
		Currency:       "USD",
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	aud := &capturingInvoiceAudit{}
	h := &Handler{
		svc:           NewService(store, nil, nil),
		charger:       &fakeCharger{},
		paymentSetups: &fakePaymentSetups{setup: domain.CustomerPaymentSetup{SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1"}},
		auditLogger:   aud,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/collect", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", inv.ID)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	req = req.WithContext(reqCtx)
	rr := httptest.NewRecorder()

	h.collectPayment(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	entry, ok := aud.firstOf("collect")
	if !ok {
		t.Fatalf("expected a 'collect' audit row; got actions=%v (middleware 'create' fallthrough not suppressed?)", aud.actions())
	}
	if entry.label != "INV-001" {
		t.Errorf("audit label: got %q, want INV-001 (so the row reads 'Collected payment on INV-001')", entry.label)
	}
	if entry.meta["amount_cents"] != int64(5000) {
		t.Errorf("audit amount_cents: got %v, want 5000", entry.meta["amount_cents"])
	}
}
