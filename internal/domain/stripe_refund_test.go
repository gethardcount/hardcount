package domain

import (
	"strings"
	"testing"
	"time"
)

func TestStripeRefundStatus(t *testing.T) {
	cases := []struct {
		s          StripeRefundStatus
		valid      bool
		noMoney    bool
		creditNote RefundStatus
	}{
		{StripeRefundPending, true, false, RefundPending},
		{StripeRefundRequiresAction, true, false, RefundPending},
		{StripeRefundSucceeded, true, false, RefundSucceeded},
		{StripeRefundFailed, true, true, RefundFailed},
		{StripeRefundCanceled, true, true, RefundFailed},
		{"reversed", false, false, RefundPending},
	}
	for _, tc := range cases {
		if tc.s.Valid() != tc.valid || tc.s.MovedNoMoney() != tc.noMoney || tc.s.CreditNoteRefundStatus() != tc.creditNote {
			t.Errorf("%q: valid=%v noMoney=%v note=%q, want %v %v %q", tc.s,
				tc.s.Valid(), tc.s.MovedNoMoney(), tc.s.CreditNoteRefundStatus(), tc.valid, tc.noMoney, tc.creditNote)
		}
	}
}

// TestInvoiceStripeRefund_SupersededBy: the row keeps the snapshot of the
// event Stripe created last, whatever order events arrive in; within one
// second the later stage of the refund wins; a redelivery is never newer.
func TestInvoiceStripeRefund_SupersededBy(t *testing.T) {
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	row := InvoiceStripeRefund{Status: StripeRefundPending, LastEventAt: at, LastStripeEventID: "evt_created"}
	snap := func(id string, d time.Duration, s StripeRefundStatus) StripeRefundSnapshot {
		return StripeRefundSnapshot{StripeEventID: id, EventAt: at.Add(d), Status: s}
	}
	cases := []struct {
		name string
		s    StripeRefundSnapshot
		want bool
	}{
		{"redelivery", snap("evt_created", time.Hour, StripeRefundFailed), false},
		{"later event", snap("evt_failed", time.Hour, StripeRefundFailed), true},
		{"older event", snap("evt_old", -time.Hour, StripeRefundSucceeded), false},
		{"same second, later stage", snap("evt_ok", 0, StripeRefundSucceeded), true},
		{"same second, same stage", snap("evt_ra", 0, StripeRefundRequiresAction), false},
	}
	for _, tc := range cases {
		if got := row.SupersededBy(tc.s); got != tc.want {
			t.Errorf("%s: SupersededBy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStripeRefundSnapshot_SeenLate(t *testing.T) {
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		after time.Duration
		want  bool
	}{{time.Minute, false}, {StripeRefundLateAfter, false}, {StripeRefundLateAfter + time.Second, true}, {72 * time.Hour, true}} {
		s := StripeRefundSnapshot{CreatedAt: created, EventAt: created.Add(tc.after)}
		if got := s.SeenLate(); got != tc.want {
			t.Errorf("seen %s after creation: SeenLate = %v, want %v", tc.after, got, tc.want)
		}
	}
	if (StripeRefundSnapshot{EventAt: created}).SeenLate() {
		t.Error("a refund of unknown age is not late")
	}
}

// TestClassifyRefundedInStripe: a flagged refund made in Stripe pierces the
// paid-invoice early return as a Warning, names the refund and the way to
// record or dismiss it, and says why it was not recorded.
func TestClassifyRefundedInStripe(t *testing.T) {
	inv := Invoice{ID: "inv_1", Status: InvoicePaid, PaymentStatus: PaymentSucceeded, Currency: "USD"}
	if att := ClassifyInvoiceAttention(inv, AttentionContext{}); att != nil {
		t.Fatalf("a paid invoice with nothing to report raised %+v", att)
	}
	reasons := map[StripeRefundReviewReason]string{
		StripeRefundReviewInvoiceNotPaid:         "not paid",
		StripeRefundReviewCurrencyMismatch:       "EUR",
		StripeRefundReviewPrepaidCommit:          "commit relief",
		StripeRefundReviewSeenLate:               "more than a day",
		StripeRefundReviewCreditNoteRefundFailed: "card refund that failed",
		StripeRefundReviewExceedsCardRefundable:  "card payment",
		StripeRefundReviewExceedsCreditable:      "left to credit",
	}
	for reason, word := range reasons {
		r := InvoiceStripeRefund{
			StripeRefundID: "re_1", StripePaymentIntentID: "pi_1", AmountCents: 4000, Currency: "EUR",
			Outcome: StripeRefundNeedsReview, ReviewReason: reason, RefundCreatedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
		}
		att := ClassifyInvoiceAttention(inv, AttentionContext{StripeRefundReview: &r})
		if att == nil {
			t.Fatalf("%s: no attention on a paid invoice", reason)
		}
		if att.Reason != AttentionReasonRefundedInStripe || att.Severity != AttentionSeverityWarning {
			t.Errorf("%s: %s/%s, want refunded_in_stripe/warning", reason, att.Reason, att.Severity)
		}
		if att.Code != "payment.refunded_in_stripe."+string(reason) {
			t.Errorf("%s: code %q", reason, att.Code)
		}
		if !strings.Contains(att.Message, word) || !strings.Contains(att.Message, "40.00 EUR") {
			t.Errorf("%s: message %q should name %q and the amount", reason, att.Message, word)
		}
		if !strings.Contains(att.Detail, "re_1") || !strings.Contains(att.Detail, "/v1/invoices/inv_1/stripe-refunds/re_1/dismiss") {
			t.Errorf("%s: detail %q should name the refund and the dismiss route", reason, att.Detail)
		}
		if att.Since == nil || !att.Since.Equal(r.RefundCreatedAt) {
			t.Errorf("%s: since %v, want the refund's creation", reason, att.Since)
		}
	}
	// A lost dispute is Critical and wins.
	lost := InvoiceDispute{StripeDisputeID: "du_1", Status: DisputeLost, AmountCents: 100, Currency: "USD"}
	r := InvoiceStripeRefund{Outcome: StripeRefundNeedsReview, ReviewReason: StripeRefundReviewSeenLate, Currency: "USD", AmountCents: 100}
	if att := ClassifyInvoiceAttention(inv, AttentionContext{LostDispute: &lost, StripeRefundReview: &r}); att == nil || att.Reason != AttentionReasonDisputeLost {
		t.Errorf("lost dispute and flagged refund: %+v, want dispute_lost", att)
	}
}

// TestClassifyRefundedInStripe_OnAnAnomalyInvoice: an invoice with a
// payment anomaly keeps the anomaly's notice, and the notice also names the
// flagged refund made in Stripe, first, so the refund is never hidden behind
// the anomaly.
func TestClassifyRefundedInStripe_OnAnAnomalyInvoice(t *testing.T) {
	inv := Invoice{ID: "inv_1", Status: InvoicePaid, PaymentStatus: PaymentSucceeded, Currency: "USD",
		PaymentAnomalyKind: "amount_mismatch", PaymentAnomalyPaymentIntentID: "pi_1"}
	plain := ClassifyInvoiceAttention(inv, AttentionContext{})
	if plain == nil || plain.Reason != AttentionReasonPaymentAnomaly {
		t.Fatalf("anomaly invoice attention = %+v, want payment_anomaly", plain)
	}
	r := InvoiceStripeRefund{StripeRefundID: "re_1", StripePaymentIntentID: "pi_1", AmountCents: 11000, Currency: "USD",
		Outcome: StripeRefundNeedsReview, ReviewReason: StripeRefundReviewExceedsCardRefundable, RefundCreatedAt: time.Now()}
	att := ClassifyInvoiceAttention(inv, AttentionContext{StripeRefundReview: &r})
	if att == nil || att.Reason != AttentionReasonPaymentAnomaly || att.Code != plain.Code {
		t.Fatalf("attention = %+v, want the anomaly's reason and code", att)
	}
	if !strings.HasPrefix(att.Message, "A refund of 110.00 USD was made in Stripe and not recorded here") || !strings.Contains(att.Message, "also flagged a payment") {
		t.Errorf("message = %q, want the refund first, then the anomaly", att.Message)
	}
	if !strings.Contains(att.Detail, "re_1") || !strings.Contains(att.Detail, plain.Detail) {
		t.Errorf("detail = %q, want the refund's and the anomaly's", att.Detail)
	}
	if len(att.Actions) != len(plain.Actions) {
		t.Errorf("actions = %v, want the anomaly's %v", att.Actions, plain.Actions)
	}
}
