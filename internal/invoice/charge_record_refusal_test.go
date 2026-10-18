package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// The charge record over HTTP (ADR-127): a charge sent to Stripe whose result
// is not saved. The payment status is still pending, so only the record says a
// payment may be on its way. Void, Collect payment and both invoice emails
// answer the in-flight conflict, and the message says how long the wait is.
func TestChargeRecordRefusalsOverHTTP(t *testing.T) {
	started := time.Now().UTC().Add(-20 * time.Minute)
	recorded := domain.Invoice{
		InvoiceNumber:   "INV-RECORDED",
		Status:          domain.InvoiceFinalized,
		PaymentStatus:   domain.PaymentPending,
		AmountDueCents:  5000,
		CustomerID:      "cus_1",
		Currency:        "USD",
		ChargeStartedAt: &started,
	}

	for _, tc := range []struct {
		name    string
		body    string
		handler func(h *Handler) http.HandlerFunc
	}{
		{"void", "{}", func(h *Handler) http.HandlerFunc { return h.void }},
		{"collect", "{}", func(h *Handler) http.HandlerFunc { return h.collectPayment }},
		{"send", `{"email":"a@b.test"}`, func(h *Handler) http.HandlerFunc { return h.sendEmail }},
		{"resend-setup-link", "{}", func(h *Handler) http.HandlerFunc { return h.resendSetupLink }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newMemStore()
			inv, err := store.Create(ctx, "t1", recorded)
			if err != nil {
				t.Fatalf("seed invoice: %v", err)
			}
			if inv.ChargeStartedAt == nil {
				t.Fatal("setup: the in-memory store dropped the charge record")
			}
			charger := &fakeCharger{}
			h := &Handler{
				svc:           NewService(store, nil, nil),
				charger:       charger,
				paymentSetups: &fakePaymentSetups{setup: domain.CustomerPaymentSetup{SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1"}},
				auditLogger:   &capturingInvoiceAudit{},
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/x", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", inv.ID)
			reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
			reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
			req = req.WithContext(reqCtx)
			rr := httptest.NewRecorder()

			tc.handler(h)(rr, req)

			if rr.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409 while a charge is recorded. body=%s", rr.Code, rr.Body.String())
			}
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode body %q: %v", rr.Body.String(), err)
			}
			if envelope.Error.Code != "payment_in_flight" {
				t.Errorf("code %q, want payment_in_flight", envelope.Error.Code)
			}
			if !strings.Contains(envelope.Error.Message, "about an hour") {
				t.Errorf("message %q does not say an interrupted charge is confirmed within about an hour", envelope.Error.Message)
			}
		})
	}
}
