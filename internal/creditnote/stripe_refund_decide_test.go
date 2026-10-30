package creditnote

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// paidCardInvoice is a $108.25 invoice, paid in full by card on
// pi_own.
func paidCardInvoice() domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
		TotalAmountCents: 10825, AmountPaidCents: 10825, StripePaymentIntentID: "pi_own",
	}
}

func dashboardSnap(amount int64) domain.StripeRefundSnapshot {
	created := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	return domain.StripeRefundSnapshot{
		StripeEventID: "evt_1", EventAt: created.Add(time.Minute), StripeRefundID: "re_1",
		StripePaymentIntentID: "pi_own", AmountCents: amount, Currency: "usd",
		Status: domain.StripeRefundSucceeded, CreatedAt: created,
	}
}

// TestDecideStripeRefund walks every check of ADR-134's D1 for a first
// sighting: each failing check flags (or skips) the refund, and only a refund
// that passes all of them becomes a credit note.
func TestDecideStripeRefund(t *testing.T) {
	issued := func(total, refund int64) domain.CreditNote {
		return domain.CreditNote{Status: domain.CreditNoteIssued, TotalCents: total, RefundAmountCents: refund}
	}
	cases := []struct {
		name      string
		inv       func(*domain.Invoice)
		snap      func(*domain.StripeRefundSnapshot)
		commit    bool
		existing  []domain.CreditNote
		firstSeen func(domain.StripeRefundSnapshot) time.Time
		want      stripeRefundDecision
	}{
		{name: "full refund of the invoice's own payment", want: stripeRefundDecision{outcome: domain.StripeRefundRecordedAsCreditNote}},
		{name: "pending refund waits", snap: func(s *domain.StripeRefundSnapshot) { s.Status = domain.StripeRefundPending },
			want: stripeRefundDecision{outcome: domain.StripeRefundAwaiting}},
		{name: "requires_action refund waits", snap: func(s *domain.StripeRefundSnapshot) { s.Status = domain.StripeRefundRequiresAction },
			want: stripeRefundDecision{outcome: domain.StripeRefundAwaiting}},
		{name: "succeeded a day after a pending first sighting: the rule counts from the first sighting",
			snap:      func(s *domain.StripeRefundSnapshot) { s.EventAt = s.CreatedAt.Add(30 * time.Hour) },
			firstSeen: func(s domain.StripeRefundSnapshot) time.Time { return s.CreatedAt.Add(time.Minute) },
			want:      stripeRefundDecision{outcome: domain.StripeRefundRecordedAsCreditNote}},
		{name: "first seen late, pending, succeeded later",
			snap:      func(s *domain.StripeRefundSnapshot) { s.EventAt = s.CreatedAt.Add(60 * time.Hour) },
			firstSeen: func(s domain.StripeRefundSnapshot) time.Time { return s.CreatedAt.Add(30 * time.Hour) },
			want:      stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewSeenLate}},
		{name: "a note's card refund failed: flagged, whatever the caps say",
			existing: []domain.CreditNote{{Status: domain.CreditNoteIssued, TotalCents: 4000, RefundAmountCents: 4000, RefundStatus: domain.RefundFailed}},
			snap:     func(s *domain.StripeRefundSnapshot) { s.AmountCents = 4000 },
			want:     stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewCreditNoteRefundFailed}},
		{name: "failed at first sighting", snap: func(s *domain.StripeRefundSnapshot) { s.Status = domain.StripeRefundFailed },
			want: stripeRefundDecision{outcome: domain.StripeRefundNotRefunded}},
		{name: "canceled at first sighting", snap: func(s *domain.StripeRefundSnapshot) { s.Status = domain.StripeRefundCanceled },
			want: stripeRefundDecision{outcome: domain.StripeRefundNotRefunded}},
		{name: "a second payment on a paid invoice", snap: func(s *domain.StripeRefundSnapshot) { s.StripePaymentIntentID = "pi_dup" },
			want: stripeRefundDecision{outcome: domain.StripeRefundOtherPayment}},
		{name: "the anomaly payment on an unpaid invoice",
			inv: func(i *domain.Invoice) {
				i.Status, i.PaymentStatus, i.PaymentAnomalyPaymentIntentID = domain.InvoiceFinalized, domain.PaymentPending, "pi_dup"
			},
			snap: func(s *domain.StripeRefundSnapshot) { s.StripePaymentIntentID = "pi_dup" },
			want: stripeRefundDecision{outcome: domain.StripeRefundOtherPayment}},
		{name: "money on a voided invoice",
			inv:  func(i *domain.Invoice) { i.Status, i.StripePaymentIntentID = domain.InvoiceVoided, "" },
			want: stripeRefundDecision{outcome: domain.StripeRefundOtherPayment}},
		{name: "invoice not paid yet, young refund: redeliver",
			inv:  func(i *domain.Invoice) { i.Status, i.PaymentStatus = domain.InvoiceFinalized, domain.PaymentProcessing },
			snap: func(s *domain.StripeRefundSnapshot) { s.Young = true },
			want: stripeRefundDecision{retry: true}},
		{name: "invoice not paid, old refund",
			inv:  func(i *domain.Invoice) { i.Status, i.PaymentStatus = domain.InvoiceFinalized, domain.PaymentPending },
			want: stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewInvoiceNotPaid}},
		{name: "a charge attempt on an unpaid invoice, old",
			inv:  func(i *domain.Invoice) { i.Status, i.PaymentStatus = domain.InvoiceFinalized, domain.PaymentFailed },
			snap: func(s *domain.StripeRefundSnapshot) { s.StripePaymentIntentID = "pi_attempt" },
			want: stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewInvoiceNotPaid}},
		{name: "another currency", snap: func(s *domain.StripeRefundSnapshot) { s.Currency = "eur" },
			want: stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewCurrencyMismatch}},
		{name: "prepaid commit invoice", commit: true,
			want: stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewPrepaidCommit}},
		{name: "first seen more than a day after it was made",
			snap: func(s *domain.StripeRefundSnapshot) { s.EventAt = s.CreatedAt.Add(48 * time.Hour) },
			want: stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewSeenLate}},
		{name: "first seen just under a day after",
			snap: func(s *domain.StripeRefundSnapshot) { s.EventAt = s.CreatedAt.Add(23 * time.Hour) },
			want: stripeRefundDecision{outcome: domain.StripeRefundRecordedAsCreditNote}},
		{name: "more than the card payment: an amount-mismatch charge",
			snap: func(s *domain.StripeRefundSnapshot) { s.AmountCents = 11000 },
			want: stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewExceedsCardRefundable}},
		{name: "after a Hardcount refund, what is left fits",
			existing: []domain.CreditNote{issued(6825, 6825)},
			snap:     func(s *domain.StripeRefundSnapshot) { s.AmountCents = 4000 },
			want:     stripeRefundDecision{outcome: domain.StripeRefundRecordedAsCreditNote}},
		{name: "after a Hardcount refund, more than what is left",
			existing: []domain.CreditNote{issued(6825, 6825)},
			want:     stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewExceedsCardRefundable}},
		{name: "a voided note does not count",
			existing: []domain.CreditNote{{Status: domain.CreditNoteVoided, TotalCents: 10825, RefundAmountCents: 10825}},
			want:     stripeRefundDecision{outcome: domain.StripeRefundRecordedAsCreditNote}},
		{name: "after a credit-balance note: the card cap passes, the creditable cap fails",
			existing: []domain.CreditNote{issued(5000, 0)},
			want:     stripeRefundDecision{outcome: domain.StripeRefundNeedsReview, reviewReason: domain.StripeRefundReviewExceedsCreditable}},
		{name: "partly paid by credits: the card part is all Stripe can refund",
			inv:  func(i *domain.Invoice) { i.AmountPaidCents, i.CreditsAppliedCents = 6825, 4000 },
			snap: func(s *domain.StripeRefundSnapshot) { s.AmountCents = 6825 },
			want: stripeRefundDecision{outcome: domain.StripeRefundRecordedAsCreditNote}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := paidCardInvoice()
			if tc.inv != nil {
				tc.inv(&inv)
			}
			snap := dashboardSnap(10825)
			if tc.snap != nil {
				tc.snap(&snap)
			}
			firstSeen := snap.EventAt
			if tc.firstSeen != nil {
				firstSeen = tc.firstSeen(snap)
			}
			got, err := decideStripeRefund(inv, snap, firstSeen, tc.commit, tc.existing)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			if got != tc.want {
				t.Errorf("decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStripeRefundCreditNoteReason(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "Refunded in Stripe",
		"requested_by_customer": "Refunded in Stripe: requested by customer",
		"duplicate":             "Refunded in Stripe: duplicate",
	} {
		if got := stripeRefundCreditNoteReason(in); got != want {
			t.Errorf("reason(%q) = %q, want %q", in, got, want)
		}
	}
}
