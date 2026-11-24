package payment

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

func processingInvoice() (*mockInvoiceUpdater, *mockWebhookStore) {
	invoices := newMockInvoiceUpdater()
	invoices.invoices["inv_1"] = domain.Invoice{
		ID: "inv_1", TenantID: "t1", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentProcessing, StripePaymentIntentID: "pi_ev",
	}
	invoices.byPI["pi_ev"] = "inv_1"
	return invoices, newMockWebhookStore()
}

var succeeded = domain.StripeWebhookEvent{StripeEventID: "evt_ev", EventType: "payment_intent.succeeded", PaymentIntentID: "pi_ev"}

// A test-mode payment event is paid at the machine's time.
func TestHandleWebhook_APaymentIsPaidAtTheMachinesTime(t *testing.T) {
	invoices, webhooks := processingInvoice()
	s := NewStripe(&mockStripeClient{}, invoices, webhooks, nil)
	if err := s.HandleWebhook(postgres.WithLivemode(context.Background(), false), "t1", succeeded); err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if got := invoices.invoices["inv_1"]; got.PaidAt == nil || time.Since(*got.PaidAt) > time.Minute {
		t.Errorf("paid_at %v; want the machine's now", got.PaidAt)
	}
}

const setupIntentRaw = `{"id": "evt_seti_ev", "type": "setup_intent.succeeded", "data": {"object": {
	"id": "seti_ev", "payment_method": "pm_ev", "customer": "cus_stripe_ev",
	"metadata": {"hardcount_tenant_id": "t1", "hardcount_customer_id": "cus_ev"}}}}`
