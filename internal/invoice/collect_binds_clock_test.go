package invoice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// ctxCapturingCharger records the ctx the charge ran under.
type ctxCapturingCharger struct{ ctx context.Context }

func (c *ctxCapturingCharger) ChargeInvoiceNow(ctx context.Context, _ string, inv domain.Invoice, _ domain.ChargeHold, _, _ string) (domain.Invoice, error) {
	c.ctx = ctx
	return inv, nil
}

// Collect charges on the request's instant: the API binds it for
// POST /v1/invoices/{id}/collect, and the charge runs on it.
func TestCollectPayment_ChargesOnTheRequestsInstant(t *testing.T) {
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
	svc := NewService(store, nil, newMemNumberer())
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
	// The request's time, as the API binds it.
	reqCtx := auth.WithTenantID(clock.WithEffectiveNow(postgres.WithLivemode(ctx, false), frozen), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.collectPayment(rr, req.WithContext(reqCtx))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	if charger.ctx == nil {
		t.Fatal("charge was not attempted")
	}
	if at, ok := clock.EffectiveNow(charger.ctx); !ok || !at.Equal(frozen) {
		t.Errorf("charge ctx instant = %v (bound %v), want the request's %v", at, ok, frozen)
	}
}
