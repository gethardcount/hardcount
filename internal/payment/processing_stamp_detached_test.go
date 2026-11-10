package payment

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// lateProcessingClient answers the confirm once the caller's context is done:
// Stripe's "processing" reply arriving just as the caller's deadline ends.
type lateProcessingClient struct{ mockStripeClient }

func (c *lateProcessingClient) CreateUnconfirmedPaymentIntent(context.Context, PaymentIntentParams) (PaymentIntentResult, error) {
	return PaymentIntentResult{ID: "pi_late", Status: "requires_confirmation"}, nil
}

func (c *lateProcessingClient) ConfirmPaymentIntent(ctx context.Context, _ string, _ PaymentIntentParams) (PaymentIntentResult, error) {
	<-ctx.Done()
	return PaymentIntentResult{ID: "pi_late", Status: "processing"}, nil
}

// TestChargeInvoice_ProcessingStampSurvivesTheCallersDeadline: the
// processing stamp is saved even when the caller's deadline (dunning's 15
// seconds) ended while Stripe answered. Stripe has the PaymentIntent; before,
// the stamp failed on the expired context, the invoice did not show the
// payment, and the retry was skipped as if nothing had reached Stripe. A
// caller that was cancelled (a shutdown) saves no answer, and needs none: the
// invoice was stamped unknown on the PaymentIntent before the confirm, and
// the unknown sweep reads it.
func TestChargeInvoice_ProcessingStampSurvivesTheCallersDeadline(t *testing.T) {
	t.Run("deadline: the stamp is saved", func(t *testing.T) {
		invoices := &ctxCheckingUpdater{mockInvoiceUpdater: newMockInvoiceUpdater()}
		invoices.invoices["inv_1"] = finalizedPendingInvoice()
		s := NewStripe(&lateProcessingClient{}, invoices, newMockWebhookStore(), nil)
		ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), true), 20*time.Millisecond)
		defer cancel()

		got, err := s.ChargeInvoiceForDunningRetry(ctx, "t1", invoices.invoices["inv_1"], testHold, "cus_stripe_abc", "pm_test")
		if err != nil {
			t.Fatalf("charge: %v, want the processing stamp saved", err)
		}
		if got.PaymentStatus != domain.PaymentProcessing || got.StripePaymentIntentID != "pi_late" {
			t.Errorf("returned %s/%q, want processing/pi_late", got.PaymentStatus, got.StripePaymentIntentID)
		}
		if stored := invoices.invoices["inv_1"]; stored.PaymentStatus != domain.PaymentProcessing || stored.StripePaymentIntentID != "pi_late" {
			t.Errorf("stored %s/%q, want processing/pi_late", stored.PaymentStatus, stored.StripePaymentIntentID)
		}
	})

	t.Run("cancelled: the invoice already says unknown on the PaymentIntent", func(t *testing.T) {
		invoices := &ctxCheckingUpdater{mockInvoiceUpdater: newMockInvoiceUpdater()}
		invoices.invoices["inv_1"] = finalizedPendingInvoice()
		s := NewStripe(&lateProcessingClient{}, invoices, newMockWebhookStore(), nil)
		ctx, cancel := context.WithCancel(postgres.WithLivemode(context.Background(), true))
		time.AfterFunc(20*time.Millisecond, cancel)

		if _, err := s.ChargeInvoiceForDunningRetry(ctx, "t1", invoices.invoices["inv_1"], testHold, "cus_stripe_abc", "pm_test"); err == nil {
			t.Fatal("charge returned no error on a cancelled caller; want the stamp dropped")
		}
		if stored := invoices.invoices["inv_1"]; stored.PaymentStatus != domain.PaymentUnknown || stored.StripePaymentIntentID == "" {
			t.Errorf("stored %s on %q, want unknown on the charge's PaymentIntent (stamped before the confirm)", stored.PaymentStatus, stored.StripePaymentIntentID)
		}
	})
}
