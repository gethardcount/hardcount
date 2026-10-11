package invoice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// ctxCapturingCharger records the ctx the charge ran under.
type ctxCapturingCharger struct{ ctx context.Context }

func (c *ctxCapturingCharger) ChargeInvoice(ctx context.Context, _ string, inv domain.Invoice, _, _ string) (domain.Invoice, error) {
	c.ctx = ctx
	return inv, nil
}

// pinnedResolver resolves every invoice onto one test clock.
type pinnedResolver struct{ sim clock.Sim }

func (p pinnedResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return p.sim, nil
}
func (p pinnedResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	return p.sim, nil
}
func (p pinnedResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return p.sim, nil
}

// Collect on a clock-pinned invoice charges inside the invoice's simulated
// time, as finalize's collect does. The charge stamps that instant on the PI
// (velox_anchor_at); paid_at and a decline's first dunning retry are dated
// from it. Unbound, a Collect on a later Advance dated the first retry from
// the issue instant — in the past — and one Advance ran the whole campaign.
func TestCollectPayment_ChargesInTheInvoicesClock(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	inv, err := store.Create(ctx, "t1", domain.Invoice{
		InvoiceNumber: "INV-001", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		AmountDueCents: 5000, CustomerID: "cus_1", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	frozen := time.Date(2027, 6, 5, 0, 0, 0, 0, time.UTC)
	svc := NewService(store, nil, nil)
	svc.SetResolver(pinnedResolver{sim: clock.Sim{At: frozen, TestClockID: "clk_1"}})
	charger := &ctxCapturingCharger{}
	h := &Handler{
		svc:           svc,
		charger:       charger,
		paymentSetups: &fakePaymentSetups{setup: domain.CustomerPaymentSetup{SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1"}},
		auditLogger:   &capturingInvoiceAudit{},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/collect", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", inv.ID)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.collectPayment(rr, req.WithContext(reqCtx))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	if charger.ctx == nil {
		t.Fatal("charge was not attempted")
	}
	sim, ok := clock.SimOf(charger.ctx)
	if !ok || sim.TestClockID != "clk_1" || !sim.At.Equal(frozen) {
		t.Errorf("charge ctx sim = %+v (bound %v), want the invoice's clock at %v", sim, ok, frozen)
	}
}
