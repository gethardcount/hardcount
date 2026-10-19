package domain

import (
	"strings"
	"testing"
	"time"
)

// TestDisputeStatus_ValidAndClosed: the eight statuses Stripe documents for a
// Dispute are the eight Velox stores (the column's CHECK lists the same), and
// exactly won, lost, warning_closed and prevented are closed.
func TestDisputeStatus_ValidAndClosed(t *testing.T) {
	closed := map[DisputeStatus]bool{
		DisputeWarningNeedsResponse: false, DisputeWarningUnderReview: false,
		DisputeNeedsResponse: false, DisputeUnderReview: false,
		DisputeWarningClosed: true, DisputeWon: true, DisputeLost: true, DisputePrevented: true,
	}
	for s, want := range closed {
		if !s.Valid() {
			t.Errorf("%s: not valid", s)
		}
		if s.Closed() != want {
			t.Errorf("%s: closed = %v, want %v", s, s.Closed(), want)
		}
	}
	for _, s := range []DisputeStatus{"", "charge_refunded", "under_arbitration"} {
		if s.Valid() || s.Closed() {
			t.Errorf("%q: valid=%v closed=%v, want neither", s, s.Valid(), s.Closed())
		}
	}
}

// TestInvoiceDispute_SupersededBy is the ordering rule that makes the record
// insensitive to delivery order: a later event replaces the row, an earlier
// one never does, the same event never does (a redelivery), and within one
// second the later stage of the dispute's life wins.
func TestInvoiceDispute_SupersededBy(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	row := InvoiceDispute{Status: DisputeUnderReview, LastEventAt: at, LastStripeEventID: "evt_row"}
	snap := func(id string, s DisputeStatus, when time.Time) DisputeSnapshot {
		return DisputeSnapshot{StripeEventID: id, Status: s, EventAt: when}
	}
	cases := []struct {
		name string
		s    DisputeSnapshot
		want bool
	}{
		{"later event", snap("evt_b", DisputeNeedsResponse, at.Add(time.Second)), true},
		{"earlier event", snap("evt_b", DisputeLost, at.Add(-time.Second)), false},
		{"same event redelivered, even if its time were later", snap("evt_row", DisputeLost, at.Add(time.Hour)), false},
		{"same second, later stage", snap("evt_b", DisputeLost, at), true},
		{"same second, earlier stage", snap("evt_b", DisputeNeedsResponse, at), false},
		{"same second, same stage", snap("evt_b", DisputeUnderReview, at), false},
	}
	for _, tc := range cases {
		if got := row.SupersededBy(tc.s); got != tc.want {
			t.Errorf("%s: SupersededBy = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A late win: lost, then won days later.
	lost := InvoiceDispute{Status: DisputeLost, LastEventAt: at, LastStripeEventID: "evt_lost"}
	if !lost.SupersededBy(snap("evt_won", DisputeWon, at.Add(72*time.Hour))) {
		t.Error("a later won must replace lost (Stripe's late win)")
	}
	// Two closes in one second do not order each other: the first one stays.
	if lost.SupersededBy(snap("evt_won", DisputeWon, at)) {
		t.Error("won in the same second as lost must not replace it")
	}
}

func lostDispute(closedAt time.Time) *InvoiceDispute {
	return &InvoiceDispute{
		StripeDisputeID: "du_lost_1", StripePaymentIntentID: "pi_lost_1", Status: DisputeLost,
		Reason: "fraudulent", AmountCents: 4500, Currency: "USD", ClosedAt: &closedAt,
	}
}

// TestClassifyDisputeLost: a lost dispute raises dispute_lost on the paid
// invoice, through the terminal-status early return that hides every other
// payment notice there. Critical, no action button (the decision is the
// operator's), the amount in the sentence and the Stripe dispute id in
// Detail. Without a lost dispute a paid invoice stays quiet. (An anomaly on
// the same invoice, and an invoice that bought prepaid credit, have their own
// tests below.)
func TestClassifyDisputeLost(t *testing.T) {
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	paid := Invoice{ID: "vlx_inv_d", Status: InvoicePaid, PaymentStatus: PaymentSucceeded, Currency: "USD"}

	if att := ClassifyInvoiceAttention(paid, AttentionContext{}); att != nil {
		t.Fatalf("control: paid invoice without a lost dispute has attention %+v", att)
	}
	for _, status := range []InvoiceStatus{InvoicePaid, InvoiceVoided, InvoiceUncollectible, InvoiceFinalized} {
		inv := paid
		inv.Status = status
		att := ClassifyInvoiceAttention(inv, AttentionContext{LostDispute: lostDispute(closedAt)})
		if att == nil || att.Reason != AttentionReasonDisputeLost {
			t.Fatalf("%s invoice with a lost dispute: %+v, want dispute_lost", status, att)
		}
		if att.Severity != AttentionSeverityCritical || len(att.Actions) != 0 || att.Code != "payment.dispute_lost" {
			t.Errorf("%s: severity %s, actions %v, code %q; want critical, none, payment.dispute_lost", status, att.Severity, att.Actions, att.Code)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, FormatMoneyMinor(4500, "USD")) {
			t.Errorf("message %q does not state the amount taken back", att.Message)
		}
		if !strings.Contains(att.Detail, "du_lost_1") || !strings.Contains(att.Detail, "not as a card refund") {
			t.Errorf("detail %q must name the Stripe dispute and warn against a card refund", att.Detail)
		}
		if att.Since == nil || !att.Since.Equal(closedAt) {
			t.Errorf("since = %v, want the close %s", att.Since, closedAt)
		}
	}

}

// TestClassifyDisputeLost_WithPaymentAnomaly: an invoice charged twice whose
// original payment was then disputed and lost. The anomaly keeps its reason
// and its "Open in Stripe" button, but it no longer says to refund the extra
// charge: the bank already gave the original back, so that refund would
// return the customer's money a second time. The notice states the loss and
// Detail names the dispute and the disputed payment beside the anomaly's own
// facts.
func TestClassifyDisputeLost_WithPaymentAnomaly(t *testing.T) {
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	inv := Invoice{
		ID: "vlx_inv_d", Status: InvoicePaid, PaymentStatus: PaymentSucceeded, Currency: "USD",
		StripePaymentIntentID:         "pi_lost_1",
		PaymentAnomalyKind:            EventPaymentDuplicateCharge,
		PaymentAnomalyPaymentIntentID: "pi_extra_1",
	}
	control := ClassifyInvoiceAttention(inv, AttentionContext{})
	if control == nil || !strings.Contains(control.Message, "refund the extra charge") {
		t.Fatalf("control: the duplicate-charge notice without a dispute must ask for the refund, got %+v", control)
	}

	att := ClassifyInvoiceAttention(inv, AttentionContext{LostDispute: lostDispute(closedAt)})
	if att == nil || att.Reason != AttentionReasonPaymentAnomaly || att.Severity != AttentionSeverityCritical {
		t.Fatalf("anomaly + lost dispute: %+v, want a critical payment_anomaly", att)
	}
	if got := actionCodes(att); len(got) != 1 || got[0] != AttentionActionReconcilePayment {
		t.Errorf("actions = %v, want only reconcile_payment (open the payments in Stripe)", got)
	}
	checkShape(t, att.Message)
	if strings.Contains(att.Message, "refund the extra charge") {
		t.Errorf("message %q still asks for the refund the chargeback may already have paid", att.Message)
	}
	if !strings.Contains(att.Message, FormatMoneyMinor(4500, "USD")) || !strings.Contains(att.Message, "lost dispute") {
		t.Errorf("message %q must state the amount the bank took back after a lost dispute", att.Message)
	}
	for _, want := range []string{"du_lost_1", "pi_lost_1", "Extra payment: pi_extra_1"} {
		if !strings.Contains(att.Detail, want) {
			t.Errorf("detail %q does not name %q", att.Detail, want)
		}
	}
}

// TestClassifyDisputeLost_PrepaidCommit: a lost dispute on a paid invoice that
// bought prepaid credit. The bank has the money back but the credit is still
// spendable, and an ordinary credit note is refused on such an invoice. So
// the notice says how much can still be spent and names the one action that
// retires it (commit relief, as money returned outside Velox); once the
// credit is all spent there is nothing to retire and no credit note to point
// to.
func TestClassifyDisputeLost_PrepaidCommit(t *testing.T) {
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	inv := Invoice{
		ID: "vlx_inv_c", Status: InvoicePaid, PaymentStatus: PaymentSucceeded, Currency: "USD",
		BillingReason: BillingReasonManual,
	}

	t.Run("credit left", func(t *testing.T) {
		att := ClassifyInvoiceAttention(inv, AttentionContext{
			LostDispute: lostDispute(closedAt), CommitGrantDrawableCents: 6000, CommitGrantConsumedCents: 4000,
		})
		if att == nil || att.Reason != AttentionReasonDisputeLost || att.Severity != AttentionSeverityCritical {
			t.Fatalf("attention %+v, want critical dispute_lost", att)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, FormatMoneyMinor(6000, "USD")) {
			t.Errorf("message %q must say how much prepaid credit is still spendable", att.Message)
		}
		if !strings.Contains(att.Message, "commit relief") {
			t.Errorf("message %q must name commit relief, the action that retires the credit", att.Message)
		}
		for _, want := range []string{"commit_relief", "retire_all", "out_of_band_amount_cents", "du_lost_1"} {
			if !strings.Contains(att.Detail, want) {
				t.Errorf("detail %q does not name %q", att.Detail, want)
			}
		}
	})

	t.Run("credit all spent", func(t *testing.T) {
		att := ClassifyInvoiceAttention(inv, AttentionContext{
			LostDispute: lostDispute(closedAt), CommitGrantDrawableCents: 0, CommitGrantConsumedCents: 10000,
		})
		if att == nil || att.Reason != AttentionReasonDisputeLost {
			t.Fatalf("attention %+v, want dispute_lost", att)
		}
		checkShape(t, att.Message)
		if strings.Contains(att.Message, "credit note") {
			t.Errorf("message %q points to a credit note, which this invoice refuses and which has nothing to retire", att.Message)
		}
		if !strings.Contains(att.Detail, "already spent") {
			t.Errorf("detail %q must say the credit is already spent", att.Detail)
		}
	})
}
