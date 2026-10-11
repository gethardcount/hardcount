package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

// A clock-pinned invoice's first dunning retry counts from the simulated
// instant its charge fired, not its issue instant. They differ when the
// charge fires on a later Advance than the issue — a paused subscription's
// draft finalized later, a tax-pending draft finalized after its retry —
// and counting from the issue would put the first retry (and, since each
// retry chains off the last in simulated time, the whole campaign) in the
// past, so one Advance would run it to its end with no waiting at all.
//
// Every report of a decline must hand the same instant to StartDunning,
// because which report runs first is an accident of delivery: the charge's
// own response, the webhook (PI metadata) and the reconciler (retrieved PI).

var (
	simIssued  = time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC) // cycle close, invoice paused → draft
	simCharged = time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC) // finalized and charged on a later Advance
)

func simDraftFinalizedLater() domain.Invoice {
	inv := finalizedPendingInvoice()
	issued := simIssued
	inv.IssuedAt = &issued
	return inv
}

func TestDecline_ChargeResponse_StartsDunningFromTheChargeInstant(t *testing.T) {
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "card_declined", PaymentIntentID: "pi_late"}}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = simDraftFinalizedLater()
	dunning := &recordingDunningStarter{}
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)

	ctx := clock.WithSim(context.Background(), clock.Sim{At: simCharged, TestClockID: "clk_1"})
	if _, err := s.ChargeInvoice(ctx, "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test"); err == nil {
		t.Fatal("expected the decline")
	}
	if len(dunning.calls) != 1 {
		t.Fatalf("StartDunning calls = %d, want 1", len(dunning.calls))
	}
	if got := dunning.calls[0].failureAt; !got.Equal(simCharged) {
		t.Errorf("StartDunning got %v, want the charge instant %v (not the issue instant %v)", got, simCharged, simIssued)
	}
}

// A definite failure Stripe never attached a PaymentIntent to starts dunning
// from the same charge instant.
func TestDecline_NoPaymentIntent_StartsDunningFromTheChargeInstant(t *testing.T) {
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "invalid payment method"}}
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = simDraftFinalizedLater()
	dunning := &recordingDunningStarter{}
	s := NewStripe(client, invoices, newMockWebhookStore(), nil, dunning)

	ctx := clock.WithSim(context.Background(), clock.Sim{At: simCharged, TestClockID: "clk_1"})
	_, _ = s.ChargeInvoice(ctx, "t1", invoices.invoices["inv_1"], "cus_stripe_abc", "pm_test")
	if len(dunning.calls) != 1 {
		t.Fatalf("StartDunning calls = %d, want 1", len(dunning.calls))
	}
	if got := dunning.calls[0].failureAt; !got.Equal(simCharged) {
		t.Errorf("StartDunning got %v, want the charge instant %v", got, simCharged)
	}
}

func TestDecline_Webhook_StartsDunningFromThePIAnchor(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	inv := simDraftFinalizedLater()
	inv.PaymentStatus = domain.PaymentProcessing
	inv.StripePaymentIntentID = "pi_late"
	invoices.invoices["inv_1"] = inv
	invoices.byPI["pi_late"] = "inv_1"
	dunning := &recordingDunningStarter{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)

	// The webhook arrives in wall-clock time, so the anchor is all it has.
	err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID: "evt_late", EventType: "payment_intent.payment_failed",
		PaymentIntentID: "pi_late", FailureMessage: "Your card was declined.",
		Payload: rawPIEventPayload(map[string]string{"velox_anchor_at": simCharged.Format(time.RFC3339Nano)}),
	})
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(dunning.calls) != 1 {
		t.Fatalf("StartDunning calls = %d, want 1", len(dunning.calls))
	}
	if got := dunning.calls[0].failureAt; !got.Equal(simCharged) {
		t.Errorf("StartDunning got %v, want the PI anchor %v", got, simCharged)
	}
}

// A decline with no anchor (a wall-clock charge, or a PI from before the
// anchor existed) falls back to the issue instant; StartDunning ignores it
// for a wall-clock invoice.
func TestDecline_Webhook_NoAnchorFallsBackToIssue(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	inv := simDraftFinalizedLater()
	inv.PaymentStatus = domain.PaymentProcessing
	inv.StripePaymentIntentID = "pi_old"
	invoices.invoices["inv_1"] = inv
	invoices.byPI["pi_old"] = "inv_1"
	dunning := &recordingDunningStarter{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)

	if err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID: "evt_old", EventType: "payment_intent.payment_failed",
		PaymentIntentID: "pi_old", FailureMessage: "Your card was declined.",
	}); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(dunning.calls) != 1 || !dunning.calls[0].failureAt.Equal(simIssued) {
		t.Fatalf("StartDunning calls = %+v, want one at the issue instant %v", dunning.calls, simIssued)
	}
}

func TestDecline_Reconciler_CarriesThePIAnchor(t *testing.T) {
	store := newMockReconcileStore(domain.Invoice{
		ID: "inv_1", TenantID: "t1", PaymentStatus: domain.PaymentUnknown,
		StripePaymentIntentID: "pi_late",
	})
	client := &mockStripeClient{piStates: map[string]PaymentIntentResult{
		"pi_late": {ID: "pi_late", Status: "requires_payment_method", AnchorAt: simCharged},
	}}
	settler := &anchorCapturingSettler{}
	r := NewReconciler(client, store, time.Second)
	r.SetSettler(settler)

	if _, errs := r.Run(context.Background(), 10); len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if settler.failCalls != 1 {
		t.Fatalf("SettleFailed calls = %d, want 1", settler.failCalls)
	}
	if !settler.failHadAnchor || !settler.failAnchor.Equal(simCharged) {
		t.Errorf("SettleFailed anchor = %v (present %v), want %v", settler.failAnchor, settler.failHadAnchor, simCharged)
	}
}

// The same decline, delivered as Stripe delivers it: a signed body through the
// webhook HTTP handler. This is the path the hand-built-payload tests could not
// see — the handler keeps the body only as a string under Payload["raw"], and
// the anchor reader used to look for a parsed Payload["data"] instead, so the
// webhook never recovered the anchor in production (paid_at included).
func TestDecline_SignedWebhook_StartsDunningFromThePIAnchor(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	inv := simDraftFinalizedLater()
	inv.PaymentStatus = domain.PaymentProcessing
	inv.StripePaymentIntentID = "pi_late"
	invoices.invoices["inv_1"] = inv
	invoices.byPI["pi_late"] = "inv_1"
	dunning := &recordingDunningStarter{}
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, dunning)

	secret := "whsec_sim_anchor"
	handler := NewHandler(s, &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_sim": {ID: "vlx_spc_sim", TenantID: "t1", Livemode: false, WebhookSecret: secret},
	}})
	body, _ := json.Marshal(map[string]any{
		"id": "evt_late_signed", "type": "payment_intent.payment_failed", "created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{
			"id": "pi_late", "object": "payment_intent", "status": "requires_payment_method", "amount": 5000, "currency": "usd",
			"last_payment_error": map[string]any{"message": "Your card was declined."},
			"metadata": map[string]string{
				"velox_tenant_id": "t1", "velox_invoice_id": "inv_1",
				"velox_anchor_at": simCharged.Format(time.RFC3339Nano),
			},
		}},
	})
	req := httptest.NewRequest("POST", "/stripe/vlx_spc_sim", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if len(dunning.calls) != 1 {
		t.Fatalf("StartDunning calls = %d, want 1", len(dunning.calls))
	}
	if got := dunning.calls[0].failureAt; !got.Equal(simCharged) {
		t.Errorf("StartDunning got %v, want the PI anchor %v (the issue instant %v means the anchor was lost in parsing)", got, simCharged, simIssued)
	}
}

// The paid twin through the same signed path: paid_at is the charge's anchor,
// not the clock's current frozen_time. Before the parsing fix the webhook
// settle never saw the anchor.
func TestSucceeded_SignedWebhook_PaidAtIsThePIAnchor(t *testing.T) {
	invoices := newMockInvoiceUpdater()
	inv := simDraftFinalizedLater()
	inv.PaymentStatus = domain.PaymentProcessing
	inv.StripePaymentIntentID = "pi_paid"
	invoices.invoices["inv_1"] = inv
	invoices.byPI["pi_paid"] = "inv_1"
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})

	secret := "whsec_sim_anchor"
	handler := NewHandler(s, &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"vlx_spc_sim": {ID: "vlx_spc_sim", TenantID: "t1", Livemode: false, WebhookSecret: secret},
	}})
	body, _ := json.Marshal(map[string]any{
		"id": "evt_paid_signed", "type": "payment_intent.succeeded", "created": time.Now().Unix(), "livemode": false,
		"data": map[string]any{"object": map[string]any{
			"id": "pi_paid", "object": "payment_intent", "status": "succeeded", "amount": 5000, "currency": "usd",
			"metadata": map[string]string{
				"velox_tenant_id": "t1", "velox_invoice_id": "inv_1",
				"velox_anchor_at": simCharged.Format(time.RFC3339Nano),
			},
		}},
	})
	req := httptest.NewRequest("POST", "/stripe/vlx_spc_sim", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := invoices.invoices["inv_1"]
	if got.PaidAt == nil || !got.PaidAt.Equal(simCharged) {
		t.Errorf("paid_at = %v, want the PI anchor %v", got.PaidAt, simCharged)
	}
}
