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
// Detail. Without a lost dispute a paid invoice stays quiet.
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

	// A payment anomaly on the same invoice keeps its place first.
	anomalous := paid
	anomalous.PaymentAnomalyKind = EventPaymentDuplicateCharge
	if att := ClassifyInvoiceAttention(anomalous, AttentionContext{LostDispute: lostDispute(closedAt)}); att == nil || att.Reason != AttentionReasonPaymentAnomaly {
		t.Errorf("anomaly + lost dispute: %+v, want payment_anomaly first", att)
	}
}
