package payment

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestChargeInvoiceNow_RecordsChargeNowTrigger: the operator's Charge now
// (and POST /v1/invoices/{id}/collect) records its attempts as charge_now, so
// "why was this charged?" has its own answer apart from the automatic
// charges (ADR-128 §5, PR 6). Only the attempt's trigger changes: the
// PaymentIntent carries the same metadata and the same idempotency key as an
// automatic charge at the same seq, so Stripe sees exactly what it saw before.
func TestChargeInvoiceNow_RecordsChargeNowTrigger(t *testing.T) {
	cases := []struct {
		name   string
		client *mockStripeClient
		fails  bool
		want   domain.ChargeAttemptOutcome
	}{
		{"accepted", &mockStripeClient{piID: "pi_now", chargeStatus: "processing"}, false, domain.ChargeAttemptPending},
		{"declined", &mockStripeClient{shouldFail: true, failErr: &PaymentError{
			Message: "Your card was declined.", DeclineCode: "card_declined", PaymentIntentID: "pi_now",
		}}, true, domain.ChargeAttemptFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoices := newMockInvoiceUpdater()
			invoices.invoices["inv_1"] = finalizedPendingInvoice()
			s := NewStripe(tc.client, invoices, newMockWebhookStore(), nil, &recordingDunningStarter{})
			_, err := s.ChargeInvoiceNow(postgres.WithLivemode(context.Background(), true), "t1", invoices.invoices["inv_1"], testHold, "cus_stripe_abc", "pm_test")
			if (err != nil) != tc.fails {
				t.Fatalf("ChargeInvoiceNow error = %v, want failure %v", err, tc.fails)
			}
			if len(invoices.chargeAttempts) != 1 {
				t.Fatalf("%d attempt rows, want 1", len(invoices.chargeAttempts))
			}
			if a := invoices.chargeAttempts[0]; a.Trigger != domain.ChargeTriggerChargeNow || a.Outcome != tc.want {
				t.Errorf("attempt trigger %q outcome %q, want %q %q", a.Trigger, a.Outcome, domain.ChargeTriggerChargeNow, tc.want)
			}
			p := tc.client.lastParams
			if want := ChargeIdempotencyKey("inv_1", 0, ""); p.IdempotencyKey != want {
				t.Errorf("idempotency key %q, want the automatic charge's %q", p.IdempotencyKey, want)
			}
			if purpose, ok := p.Metadata["hardcount_purpose"]; ok {
				t.Errorf("PaymentIntent carries hardcount_purpose=%q; an automatic charge carries none", purpose)
			}
		})
	}
}
