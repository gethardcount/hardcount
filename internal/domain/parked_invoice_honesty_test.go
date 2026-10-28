package domain

import (
	"strings"
	"testing"
)

// TestParkedInvoiceAttentionTellsTheTruth pins the banner for an invoice whose
// charge got an unclear answer from Stripe that named no payment (ADR-107).
//
// Since REC-5 (ADR-127 amendment, 2026-10-20) Hardcount finishes such an invoice
// itself: each billing tick lists the customer's payments at Stripe, records
// one that appears, and with none 24 hours after the attempt charges the
// invoice again as usual. The banner says exactly that, as a Warning, in the
// words Sagar approved on 2026-10-19. Write-off stays, as a secondary action,
// for a customer who paid another way: telling every operator to write off an
// invoice that will be collected tomorrow would be the wrong advice.
func TestParkedInvoiceAttentionTellsTheTruth(t *testing.T) {
	parked := Invoice{
		Status:                InvoiceFinalized,
		PaymentStatus:         PaymentUnknown,
		StripePaymentIntentID: "", // the defining property
		AmountDueCents:        5000,
	}

	att := classifyPaymentUnconfirmed(parked)
	if att == nil {
		t.Fatal("a parked invoice must raise an attention")
	}
	if att.Severity != AttentionSeverityWarning {
		t.Errorf("severity = %v, want warning: Hardcount resolves it within a day, and a payment may still arrive", att.Severity)
	}
	const want = "Stripe didn't confirm this charge and may still complete it.\n" +
		"Hardcount checks Stripe every hour and records the payment if it appears.\n" +
		"If none appears within 24 hours, the invoice is charged again as usual."
	if att.Message != want {
		t.Errorf("message = %q, want the approved copy %q", att.Message, want)
	}
	all := att.Message + " " + att.Detail
	// "never charged twice" is false since REC-5: after 24 hours Hardcount charges
	// again, and if Stripe creates the original payment after that (ADR-127
	// R10) the customer has paid twice; the second payment is flagged
	// (payment.duplicate_charge), not prevented.
	for _, stale := range []string{"will not resolve on its own", "keeps searching", "can't tell", "won't charge again", "charged twice"} {
		if strings.Contains(all, stale) {
			t.Errorf("the notice still says %q, which is no longer true", stale)
		}
	}
	// Write-off stays reachable from the notice, as the action for a customer
	// who paid another way.
	if len(att.Actions) != 1 || att.Actions[0].Code != AttentionActionMarkUncollectible {
		t.Errorf("actions = %+v, want exactly [mark_uncollectible]", att.Actions)
	}
	if !strings.Contains(att.Detail, "paid another way") {
		t.Errorf("detail = %q: it must say when to write the invoice off (the customer paid another way)", att.Detail)
	}
	// And that nothing is charged meanwhile, or the wait reads as a defect.
	if !strings.Contains(att.Detail, "No new charge is made before then.") {
		t.Errorf("detail = %q: it must say no new charge is made during the 24 hours", att.Detail)
	}
}

// TestUnconfirmedWithAPaymentIntentKeepsTheWaitingCopy is the negative control.
// A charge that IS being confirmed genuinely resolves itself, so escalating it
// to critical and telling the operator to write it off would be its own lie —
// and would train them to ignore the critical case.
func TestUnconfirmedWithAPaymentIntentKeepsTheWaitingCopy(t *testing.T) {
	confirming := Invoice{
		Status:                InvoiceFinalized,
		PaymentStatus:         PaymentUnknown,
		StripePaymentIntentID: "pi_live_123",
		AmountDueCents:        5000,
	}

	att := classifyPaymentUnconfirmed(confirming)
	if att == nil {
		t.Fatal("want an attention")
	}
	if att.Severity != AttentionSeverityInfo {
		t.Errorf("severity = %v, want info — this one really is resolving itself", att.Severity)
	}
	if !strings.Contains(att.Message, "re-checks automatically") {
		t.Error("a resolvable charge should still tell the operator to wait")
	}
	if len(att.Actions) != 0 {
		t.Errorf("a charge that resolves itself must not offer a write-off, got %+v", att.Actions)
	}
	if !strings.Contains(att.Detail, "Check the payment in Stripe") {
		t.Errorf("detail must keep the stuck-for-hours advice, got %q", att.Detail)
	}
}
