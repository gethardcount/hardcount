package api

import (
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// collectEvent is a payment_intent.* event for a Collect's PaymentIntent,
// which carries no hardcount_purpose.
func (r *inFlightRig) collectEvent(eventType, pi, invoiceID, status string, lastErr map[string]any) map[string]any {
	evt := r.piEvent(eventType, pi, invoiceID, status, lastErr)
	obj := evt["data"].(map[string]any)["object"].(map[string]any)
	delete(obj["metadata"].(map[string]any), "hardcount_purpose")
	return evt
}

// clearHold clears the invoice's charge hold, as the stale-hold sweep does
// once the hold has ended.
func (r *inFlightRig) clearHold(invoiceID string) {
	r.t.Helper()
	tx, err := r.db.BeginTx(callerCtx(r.tenantID, false), postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, invoiceID); err != nil {
		r.t.Fatalf("clear hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
}

// collectProcessing puts an invoice in dunning and has an operator's Collect
// payment come back from Stripe as processing. It returns the invoice and the
// Collect's PaymentIntent.
func collectProcessing(t *testing.T, r *inFlightRig, card string) (invoiceID, pi string) {
	t.Helper()
	invoiceID = r.newInvoice(r.newCustomer(card, card))
	if run := r.run(invoiceID); flowStr(run, "state") != "active" {
		t.Fatalf("setup: run after the finalize-time decline: %v, want active", run["state"])
	}
	r.stripe.nextOtherChargeProcessing(card)
	got := r.call("POST", "/v1/invoices/"+invoiceID+"/collect", nil).want(t, 200, "collect").body
	if flowStr(got, "payment_status") != "processing" {
		t.Fatalf("collect answered payment_status %v, want processing", got["payment_status"])
	}
	return invoiceID, r.stripe.piOf(card)
}

// When Stripe answers a Collect card payment with processing, nothing is
// recovered yet: the invoice's dunning run stays active, and no
// dunning.resolved is sent. Before, the Collect closed the run as recovered
// and sent dunning.resolved for an unpaid invoice; when the payment then
// failed, the invoice had no dunning run, so nothing retried it.
//
// These drive the real router, database, charge path and signed webhooks.

// The payment then fails: the run is still there, and its next retry charges
// the card on the policy's date.
func TestE2E_CollectProcessing_ThenFails_DunningGoesOn(t *testing.T) {
	r := newInFlightRig(t, "Collect In Flight Fails", []int{3, 3, 3}, nil)
	inv, pi := collectProcessing(t, r, "pm_collect_a")

	run := r.run(inv)
	if flowStr(run, "state") != "active" {
		t.Fatalf("run after a Collect that is still processing: %v, want active", run["state"])
	}
	if n := r.resolvedEvents(inv); n != 0 {
		t.Fatalf("dunning.resolved events after a Collect that is still processing: %d, want none: the invoice is not paid", n)
	}

	r.webhook(r.collectEvent("payment_intent.payment_failed", pi, inv, "requires_payment_method",
		cardError("insufficient_funds", "Your card has insufficient funds.", "pm_collect_a"))).want(t, 200, "payment_intent.payment_failed")
	if got := r.invoice(inv); flowStr(got, "payment_status") != "failed" {
		t.Fatalf("invoice after the failure webhook: payment_status %v, want failed", got["payment_status"])
	}
	if run := r.run(inv); flowStr(run, "state") != "active" {
		t.Fatalf("run after the Collect's payment failed: %v, want active: dunning must go on", run["state"])
	}
	// The Collect's charge hold outlives the failure until the stale-hold
	// sweep clears it; the due-run picker skips a held invoice until then.
	// Clear it as the sweep does.
	r.clearHold(inv)

	// The policy's first retry is three days after the first failure.
	r.advance(4)
	if cards := r.stripe.retryCards(); len(cards) != 1 || cards[0] != "pm_collect_a" {
		t.Fatalf("dunning retry charges after the retry date: %v, want one, on the customer's card", cards)
	}
	if n := r.resolvedEvents(inv); n != 1 {
		// The retry is answered "paid" by the stand-in, so the run resolves now.
		t.Errorf("dunning.resolved events after the retry paid: %d, want 1", n)
	}
}

// The payment then succeeds: the settle resolves the run as recovered, once.
func TestE2E_CollectProcessing_ThenSucceeds_ResolvedOnce(t *testing.T) {
	r := newInFlightRig(t, "Collect In Flight Pays", []int{3, 3, 3}, nil)
	inv, pi := collectProcessing(t, r, "pm_collect_b")
	if n := r.resolvedEvents(inv); n != 0 {
		t.Fatalf("dunning.resolved events while the Collect is processing: %d, want none", n)
	}

	r.webhook(r.collectEvent("payment_intent.succeeded", pi, inv, "succeeded", nil)).want(t, 200, "payment_intent.succeeded")
	if got := r.invoice(inv); flowStr(got, "status") != "paid" {
		t.Fatalf("invoice after the success webhook: %v, want paid", got["status"])
	}
	run := r.run(inv)
	if flowStr(run, "state") != "resolved" || flowStr(run, "resolution") != "payment_recovered" {
		t.Errorf("run: state=%v resolution=%v, want resolved / payment_recovered", run["state"], run["resolution"])
	}
	r.webhook(r.collectEvent("payment_intent.succeeded", pi, inv, "succeeded", nil)).want(t, 200, "payment_intent.succeeded again")
	r.advance(10)
	if n := r.resolvedEvents(inv); n != 1 {
		t.Errorf("dunning.resolved events: %d, want exactly 1", n)
	}
	if cards := r.stripe.retryCards(); len(cards) != 0 {
		t.Errorf("dunning retry charges on a paid invoice: %v, want none", cards)
	}
}
