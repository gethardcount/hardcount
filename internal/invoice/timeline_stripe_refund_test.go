package invoice

import (
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/activity"
)

// TestStripeRefundTimelineEvents: a refund made in Stripe that no credit note
// records is one row at Stripe's own time, worded by what Hardcount did with
// it; one recorded as a credit note has no row of its own, because the
// note's row tells it (ADR-134).
func TestStripeRefundTimelineEvents(t *testing.T) {
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	row := func(id string, o domain.StripeRefundOutcome) domain.InvoiceStripeRefund {
		r := domain.InvoiceStripeRefund{
			ID: id, StripeRefundID: "re_" + id, StripePaymentIntentID: "pi_1", AmountCents: 4000, Currency: "USD",
			Outcome: o, RefundCreatedAt: at,
		}
		switch o {
		case domain.StripeRefundNeedsReview:
			r.ReviewReason = domain.StripeRefundReviewSeenLate
		case domain.StripeRefundDismissed:
			r.ReviewReason, r.DismissedNote = domain.StripeRefundReviewExceedsCreditable, "took the credit back"
		case domain.StripeRefundNotRefunded:
			r.Status, r.FailureReason = domain.StripeRefundFailed, "expired_or_canceled_card"
		}
		return r
	}
	events := stripeRefundTimelineEvents([]domain.InvoiceStripeRefund{
		row("note", domain.StripeRefundRecordedAsCreditNote),
		row("other", domain.StripeRefundOtherPayment),
		row("review", domain.StripeRefundNeedsReview),
		row("none", domain.StripeRefundNotRefunded),
		row("dismissed", domain.StripeRefundDismissed),
	})
	if len(events) != 4 {
		t.Fatalf("rows = %d, want 4 (the recorded refund shows as its credit note)", len(events))
	}
	want := []struct {
		id, title string
		tone      activity.Tone
		amount    bool
		problem   string
	}{
		{"stripe_refund:other", "Extra payment refunded in Stripe", activity.ToneNeutral, true, ""},
		{"stripe_refund:review", "Refunded in Stripe", activity.ToneWarning, true, "Not recorded here: Hardcount heard of it more than a day late."},
		{"stripe_refund:none", "Refund in Stripe did not go through", activity.ToneNeutral, false, ""},
		{"stripe_refund:dismissed", "Refunded in Stripe", activity.ToneNeutral, true, ""},
	}
	for i, w := range want {
		e := events[i]
		if e.ID != w.id || e.Line.Title != w.title || e.Line.Tone != w.tone || (e.Line.Amount != nil) != w.amount || e.Line.Problem != w.problem {
			t.Errorf("row %d = %s %q %s amount=%v problem=%q, want %s %q %s amount=%v problem=%q", i,
				e.ID, e.Line.Title, e.Line.Tone, e.Line.Amount != nil, e.Line.Problem, w.id, w.title, w.tone, w.amount, w.problem)
		}
		if e.Source != "stripe_refund" || e.EventType != domain.EventPaymentRefundedInStripe || !e.sortAt.Equal(at) || e.PaymentIntentID != "pi_1" {
			t.Errorf("row %d: source %q type %q at %v pi %q", i, e.Source, e.EventType, e.sortAt, e.PaymentIntentID)
		}
	}
	var dismissed string
	for _, seg := range events[3].Line.Detail {
		for _, p := range seg {
			dismissed += p.Text
		}
	}
	if !strings.Contains(dismissed, "took the credit back") {
		t.Errorf("dismissed row detail %q should carry the operator's note", dismissed)
	}
}

// TestCreditNoteLine_RecordedFromStripe: a credit note that records a refund
// made in Stripe is titled for the refund.
func TestCreditNoteLine_RecordedFromStripe(t *testing.T) {
	cn := domain.CreditNote{ID: "cn_1", CreditNoteNumber: "CN-1", TotalCents: 4000, RefundAmountCents: 4000,
		Currency: "USD", RefundStatus: domain.RefundSucceeded, Reason: "Refunded in Stripe"}
	if got := creditNoteLine(cn, nil, "").Title; got != "Credit note issued" {
		t.Errorf("ordinary note title %q", got)
	}
	cn.RecordedFromStripe = true
	l := creditNoteLine(cn, nil, "")
	if l.Title != "Refunded in Stripe" || l.Kind != "credit_note.issued" || l.Problem != "" {
		t.Errorf("recorded note line = %q %q problem %q, want \"Refunded in Stripe\", credit_note.issued, no problem", l.Title, l.Kind, l.Problem)
	}
}
