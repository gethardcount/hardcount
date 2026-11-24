package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
)

// TestInFlightRefusalsAnswerOneCode pins the HTTP half of the single-source
// rule. domain.PaymentBlocksAction returns a (code, message) pair, and every
// handler that refuses an invoice whose charge is unresolved must forward
// both: a handler that passes the message through and drops the code leaves
// an API consumer unable to branch on it uniformly.
func TestInFlightRefusalsAnswerOneCode(t *testing.T) {
	inFlight := domain.Invoice{
		InvoiceNumber:         "INV-INFLIGHT",
		Status:                domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentUnknown,
		StripePaymentIntentID: "pi_1",
		AmountDueCents:        5000,
		CustomerID:            "cus_1",
		Currency:              "USD",
	}
	cases := []struct {
		name    string
		body    string
		handler func(h *Handler) http.HandlerFunc
	}{
		{"void", "{}", func(h *Handler) http.HandlerFunc { return h.void }},
		{"collect", "{}", func(h *Handler) http.HandlerFunc { return h.collectPayment }},
		// send validates its payload before reaching the gate, so it needs one.
		{"send", `{"email":"a@b.test"}`, func(h *Handler) http.HandlerFunc { return h.sendEmail }},
		{"resend-setup-link", "{}", func(h *Handler) http.HandlerFunc { return h.resendSetupLink }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newMemStore()
			inv, err := store.Create(ctx, "t1", inFlight)
			if err != nil {
				t.Fatalf("seed invoice: %v", err)
			}

			h := &Handler{
				svc:           NewService(store, nil, newMemNumberer()),
				charger:       &fakeCharger{},
				paymentSetups: &fakePaymentSetups{setup: domain.CustomerPaymentSetup{SetupStatus: domain.PaymentSetupReady, StripeCustomerID: "cus_stripe_1"}},
				auditLogger:   &capturingInvoiceAudit{},
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+inv.ID+"/x", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", inv.ID)
			reqCtx := auth.WithTenantID(req.Context(), "t1")
			reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
			req = req.WithContext(reqCtx)
			rr := httptest.NewRecorder()

			tc.handler(h)(rr, req)

			if rr.Code != http.StatusConflict {
				t.Fatalf("status: got %d, want 409: every in-flight refusal is the same conflict. body=%s", rr.Code, rr.Body.String())
			}

			var envelope struct {
				Error struct {
					Type    string `json:"type"`
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode body %q: %v", rr.Body.String(), err)
			}
			// collect, send and resend-setup-link used to answer type
			// "invalid_state", a type no other endpoint uses; void answered
			// invalid_request_error.
			if envelope.Error.Type != "invalid_request_error" {
				t.Errorf("type: got %q, want invalid_request_error", envelope.Error.Type)
			}
			if envelope.Error.Code != "payment_in_flight" {
				t.Errorf("code: got %q, want payment_in_flight: a consumer branches on this, so one handler answering differently makes the single source useless over HTTP", envelope.Error.Code)
			}
		})
	}
}
