package invoice

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

// Collect refuses a card the bank refused for good on this invoice (#966;
// decided 2026-10-22): the charge cannot succeed, and the card networks charge
// merchants for retrying it. Dunning retries already skip the card; Collect
// charged it anyway. Nothing reaches Stripe and the charge hold is released.
// Any other last decline, including one that asks the customer to confirm the
// payment, still charges.
func TestCollectPayment_RefusesACardRefusedForGood(t *testing.T) {
	for _, tc := range []struct {
		lastDecline string
		wantCode    int
	}{
		{"lost_card", http.StatusConflict},
		{"stolen_card", http.StatusConflict},
		{"insufficient_funds", http.StatusOK},
		{"authentication_required", http.StatusOK},
		{"", http.StatusOK},
	} {
		t.Run("last decline "+tc.lastDecline, func(t *testing.T) {
			ctx := context.Background()
			store := newMemStore()
			inv, err := store.Create(ctx, "t1", domain.Invoice{
				InvoiceNumber: "INV-001", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				AmountDueCents: 5000, CustomerID: "cus_1", Currency: "USD",
			})
			if err != nil {
				t.Fatalf("seed invoice: %v", err)
			}
			svc := NewService(store, nil, nil)
			svc.SetCardRefusalReader(refusedCards{inv.ID: tc.lastDecline})
			charger := &ctxCapturingCharger{}
			h := &Handler{
				svc:     svc,
				charger: charger,
				paymentSetups: &fakePaymentSetups{setup: domain.CustomerPaymentSetup{
					SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1", StripePaymentMethodID: "pm_1",
				}},
				auditLogger: &capturingInvoiceAudit{},
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/collect", nil)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", inv.ID)
			reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
			reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
			rr := httptest.NewRecorder()
			h.collectPayment(rr, req.WithContext(reqCtx))

			if rr.Code != tc.wantCode {
				t.Fatalf("status %d, want %d; body=%s", rr.Code, tc.wantCode, rr.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				if charger.ctx == nil {
					t.Error("the card was not charged")
				}
				return
			}
			if charger.ctx != nil {
				t.Error("a card the bank refused for good was charged")
			}
			for _, want := range []string{`"code":"card_refused"`, tc.lastDecline, "setup link"} {
				if !bytes.Contains(rr.Body.Bytes(), []byte(want)) {
					t.Errorf("body lacks %q: %s", want, rr.Body.String())
				}
			}
			// The hold was released: the next charge can claim the invoice.
			if hold, err := svc.ClaimChargeForManualCollect(ctx, "t1", inv.ID); err != nil || !hold.Held() {
				t.Errorf("claim after the refusal: held %v err %v; the refusal left the charge hold set", hold.Held(), err)
			}
		})
	}
}
