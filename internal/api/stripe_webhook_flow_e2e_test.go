package api

import (
	"net/http"
	"testing"
)

// TestE2E_StripeWebhook_SignedEventsSettleOnce sends Stripe's
// payment_intent.succeeded and payment_intent.payment_failed over HTTP to
// POST /v1/webhooks/stripe/{endpoint_id}, signed with the secret saved in
// Settings → Payments, into a real database. Before this, the endpoint was
// never called over HTTP; the release checklist was the only proof.
//
// Each event changes its invoice once: Stripe delivers at least once, so a
// redelivery must leave the invoice, the queued events and the emails as they
// were. A delivery Stripe did not sign changes nothing.
func TestE2E_StripeWebhook_SignedEventsSettleOnce(t *testing.T) {
	f := newFlow(t, map[string]string{"SMTP_HOST": "smtp.flow-e2e.invalid"})
	stripe := newFakeStripe(t)
	endpointID, secret := f.connectStripe()

	cust := f.customer()
	custID := str(cust["id"])
	invA := f.finalizedInvoice(custID, 5000)
	invB := f.finalizedInvoice(custID, 7000)
	idA, idB := str(invA["id"]), str(invB["id"])
	u := uniq()
	piA, piB := "pi_flow_a_"+u, "pi_flow_b_"+u

	succeeded := paymentIntentEvent("evt_flow_paid_"+u, "payment_intent.succeeded", piA, invA, f.tenantID, "")

	// Not signed by the endpoint's secret: refused, nothing changes.
	if r := f.deliverStripeEvent(endpointID, "whsec_not_this_endpoint", succeeded); r.status != http.StatusBadRequest {
		t.Fatalf("event signed with another secret: status %d, want 400; body: %s", r.status, r.raw)
	}
	if r := f.send(http.MethodPost, "/v1/webhooks/stripe/"+endpointID, succeeded); r.status != http.StatusBadRequest {
		t.Fatalf("unsigned event: status %d, want 400; body: %s", r.status, r.raw)
	}
	if r := f.deliverStripeEvent("vlx_spc_not_an_endpoint", secret, succeeded); r.status != http.StatusNotFound {
		t.Fatalf("event to an unknown endpoint: status %d, want 404; body: %s", r.status, r.raw)
	}
	if a := f.invoice(idA); a["status"] != "finalized" || a["payment_status"] != "pending" {
		t.Fatalf("invoice after refused deliveries = %v/%v, want finalized/pending", a["status"], a["payment_status"])
	}

	// payment_intent.succeeded, delivered and then redelivered.
	for i := range 2 {
		r := f.deliverStripeEvent(endpointID, secret, succeeded)
		if r.status != http.StatusOK || (i == 0 && r.body["status"] != "processed") {
			t.Fatalf("succeeded delivery %d: status %d; body: %s", i+1, r.status, r.raw)
		}
	}
	a := f.invoice(idA)
	if a["status"] != "paid" || a["payment_status"] != "succeeded" || num(a["amount_due_cents"]) != 0 ||
		num(a["amount_paid_cents"]) != 5000 || a["stripe_payment_intent_id"] != piA || a["payment_card_last4"] != "4242" {
		t.Errorf("invoice after payment_intent.succeeded = status %v, payment_status %v, due %v, paid %v, payment %v, card %v; "+
			"want paid, succeeded, 0, 5000, %s, 4242",
			a["status"], a["payment_status"], a["amount_due_cents"], a["amount_paid_cents"], a["stripe_payment_intent_id"], a["payment_card_last4"], piA)
	}
	if rows := f.timeline(idA, "invoice.paid"); len(rows) != 1 || rows[0]["detail"] != "via Visa •••• 4242" {
		t.Errorf("timeline paid rows = %v; want one, via Visa •••• 4242", rows)
	}
	if runs := f.dunningRuns(idA); len(runs) != 1 || runs[0]["resolution"] != "payment_recovered" {
		t.Errorf("dunning runs after the payment = %v; want the run resolved as payment_recovered", runs)
	}
	// Once: one invoice.paid and one payment.succeeded event, one receipt,
	// one card lookup at Stripe.
	if paidEvents, succEvents := f.queuedEvents("invoice.paid", idA), f.queuedEvents("payment.succeeded", idA); paidEvents != 1 || succEvents != 1 {
		t.Errorf("events queued for the paid invoice: invoice.paid %d, payment.succeeded %d; want 1 each", paidEvents, succEvents)
	}
	if receipts := f.sentEmails(custID, "payment_receipt"); len(receipts) != 1 || receipts[0]["invoice_number"] != invA["invoice_number"] {
		t.Errorf("receipt emails = %v; want one, for %v", receipts, invA["invoice_number"])
	}
	if n := stripe.count("GET /v1/payment_intents/" + piA); n != 1 {
		t.Errorf("card lookups at Stripe for %s: %d, want 1", piA, n)
	}

	// payment_intent.payment_failed, delivered and then redelivered.
	failed := paymentIntentEvent("evt_flow_failed_"+u, "payment_intent.payment_failed", piB, invB, f.tenantID, "Your card was declined.")
	for i := range 2 {
		if r := f.deliverStripeEvent(endpointID, secret, failed); r.status != http.StatusOK {
			t.Fatalf("failed delivery %d: status %d; body: %s", i+1, r.status, r.raw)
		}
	}
	b := f.invoice(idB)
	if b["status"] != "finalized" || b["payment_status"] != "failed" || num(b["amount_due_cents"]) != 7000 ||
		b["stripe_payment_intent_id"] != piB || b["last_payment_error"] != "Your card was declined." {
		t.Errorf("invoice after payment_intent.payment_failed = status %v, payment_status %v, due %v, payment %v, error %v; "+
			"want finalized, failed, 7000, %s, the decline message",
			b["status"], b["payment_status"], b["amount_due_cents"], b["stripe_payment_intent_id"], b["last_payment_error"], piB)
	}
	if rows := f.timeline(idB, "charge_attempt.failed"); len(rows) != 1 || rows[0]["error"] != "Your card was declined." || rows[0]["payment_intent_id"] != piB {
		t.Errorf("timeline failed-charge rows = %v; want one for %s with the decline message", rows, piB)
	}
	if n := f.queuedEvents("payment.failed", idB); n != 1 {
		t.Errorf("payment.failed events queued: %d, want 1", n)
	}
	if notices := f.sentEmails(custID, "payment_failed"); len(notices) != 1 || notices[0]["invoice_number"] != invB["invoice_number"] {
		t.Errorf("payment-failed emails = %v; want one, for %v", notices, invB["invoice_number"])
	}

	// A late failure for the invoice that is already paid changes nothing.
	late := paymentIntentEvent("evt_flow_late_"+u, "payment_intent.payment_failed", "pi_flow_late_"+u, invA, f.tenantID, "Your card was declined.")
	if r := f.deliverStripeEvent(endpointID, secret, late); r.status != http.StatusOK {
		t.Fatalf("late failure: status %d; body: %s", r.status, r.raw)
	}
	if a := f.invoice(idA); a["status"] != "paid" || a["payment_status"] != "succeeded" || a["stripe_payment_intent_id"] != piA {
		t.Errorf("paid invoice after a late failure = %v/%v/%v, want paid/succeeded/%s", a["status"], a["payment_status"], a["stripe_payment_intent_id"], piA)
	}
	if n := f.queuedEvents("payment.failed", idA); n != 0 {
		t.Errorf("payment.failed events queued for the paid invoice: %d, want 0", n)
	}
}
