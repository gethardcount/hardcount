package payment

import (
	"context"
	"database/sql"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// recordingRetryFailure stands in for dunning's RetryPaymentFailed: it records
// what the settle passed and counts how often the hook it returns ran.
type recordingRetryFailure struct {
	calls []string // pi/declineCode/cardMessage
	ran   int
	seen  domain.Invoice
}

func (r *recordingRetryFailure) RetryPaymentFailed(_ context.Context, _ string, _ domain.Invoice, pi, code, msg string) func(tx *sql.Tx, fresh domain.Invoice) error {
	r.calls = append(r.calls, pi+"/"+code+"/"+msg)
	return func(_ *sql.Tx, fresh domain.Invoice) error {
		r.ran++
		r.seen = fresh
		return nil
	}
}

// TestSettleFailed_DunningRetryFailureRunsTheDunningHook: the failure of a
// dunning retry's PaymentIntent runs dunning's hook inside the settle's
// transaction, once per PaymentIntent, on the row the failed stamp wrote
// (#1289). No generic payment-failed email goes out for it, and any other
// purpose never reaches the hook.
func TestSettleFailed_DunningRetryFailureRunsTheDunningHook(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), true)
	invoices := newMockInvoiceUpdater()
	inv := finalizedPendingInvoice()
	inv.PaymentStatus = domain.PaymentProcessing
	inv.StripePaymentIntentID = "pi_slow"
	invoices.invoices["inv_1"] = inv
	invoices.byPI["pi_slow"] = "inv_1"
	s := NewStripe(&mockStripeClient{}, invoices, newMockWebhookStore(), nil)
	emails := &recordingFailedEmail{}
	s.SetEmailPaymentFailed(emails, staticCustomerEmail{})
	hook := &recordingRetryFailure{}
	s.SetDunningRetryFailure(hook)

	failure := PaymentFailure{
		PaymentIntentID: "pi_slow", Message: "payment failed", Purpose: piPurposeDunningRetry,
		DeclineCode: "insufficient_funds", CardMessage: "Your card has insufficient funds.",
	}
	if err := s.SettleFailed(ctx, "t1", inv, failure, SourceWebhook); err != nil {
		t.Fatalf("settle failed: %v", err)
	}
	if len(hook.calls) != 1 || hook.calls[0] != "pi_slow/insufficient_funds/Your card has insufficient funds." {
		t.Fatalf("hook calls %v, want one with the PaymentIntent, decline code and card message", hook.calls)
	}
	if hook.ran != 1 || hook.seen.PaymentStatus != domain.PaymentFailed {
		t.Errorf("hook ran %d times on %s, want once on the failed row", hook.ran, hook.seen.PaymentStatus)
	}
	if emails.sends != 0 {
		t.Errorf("generic payment-failed emails %d, want 0 for a dunning retry", emails.sends)
	}

	// The reconciler reports it again: the store's first-notice gate keeps the
	// hook from running twice.
	if err := s.SettleFailed(ctx, "t1", invoices.invoices["inv_1"], failure, SourceReconciler); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if hook.ran != 1 {
		t.Errorf("hook ran %d times after a second report, want 1", hook.ran)
	}

	// A failure of another purpose never reaches it.
	other := finalizedPendingInvoice()
	other.ID = "inv_2"
	other.PaymentStatus = domain.PaymentProcessing
	other.StripePaymentIntentID = "pi_auto"
	invoices.invoices["inv_2"] = other
	if err := s.SettleFailed(ctx, "t1", other, PaymentFailure{PaymentIntentID: "pi_auto", Message: "payment failed"}, SourceWebhook); err != nil {
		t.Fatalf("settle auto-charge failure: %v", err)
	}
	if len(hook.calls) != 2 {
		t.Errorf("hook calls %v, want only the two dunning-retry reports", hook.calls)
	}
}
