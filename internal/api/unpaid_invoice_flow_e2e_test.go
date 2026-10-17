package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// TestE2E_UnpaidInvoice_PaymentLinkThenOfflinePayment walks an invoice whose
// customer has no card, on a real database:
//
//   - finalizing it emails a payment-update link, and the customer's Sent
//     emails list shows that email (GET /customers/{id}/sent-emails);
//   - the link's public page shows the invoice, and its checkout opens one
//     Stripe setup session, after which the link is spent (the two
//     /public/payment-updates endpoints, which had no handler test);
//   - the operator then records a bank transfer (record-payment, whose
//     success path was tested only behind fakes): the invoice is paid, the
//     timeline says so, dunning stops, and a replay records nothing twice.
func TestE2E_UnpaidInvoice_PaymentLinkThenOfflinePayment(t *testing.T) {
	f := newFlow(t, map[string]string{
		// The setup-link email needs SMTP and the payment-update page
		// address. No dispatcher runs here, so nothing connects to the host.
		"SMTP_HOST":          "smtp.flow-e2e.invalid",
		"PAYMENT_UPDATE_URL": "http://pay.flow-e2e.test/update-payment",
	})
	stripe := newFakeStripe(t)
	admin := bearer(f.testKey)
	f.connectStripe()

	cust := f.customer()
	custID := str(cust["id"])
	inv := f.finalizedInvoice(custID, 5000)
	invID, invNumber := str(inv["id"]), str(inv["invoice_number"])
	if inv["status"] != "finalized" || inv["payment_status"] != "pending" || num(inv["amount_due_cents"]) != 5000 {
		t.Fatalf("finalized invoice with no card = %v; want finalized, pending, 5000 due", inv)
	}

	// No card, so dunning starts and the customer is emailed a link.
	runs := f.dunningRuns(invID)
	if len(runs) != 1 || runs[0]["state"] != "active" {
		t.Fatalf("dunning runs after finalize = %v; want one active run", runs)
	}
	sent := f.sentEmails(custID, "payment_setup_request")
	if len(sent) != 1 || sent[0]["invoice_number"] != invNumber || sent[0]["recipient"] != cust["email"] {
		t.Fatalf("Sent emails = %v; want one payment_setup_request for %s to %v", sent, invNumber, cust["email"])
	}
	token := f.emailedToken("payment_setup_request", "update_url")

	// The link's page: the invoice it is for, without signing in.
	page := f.must(http.StatusOK, http.MethodGet, "/v1/public/payment-updates/"+token, nil)
	if page["invoice_number"] != invNumber || num(page["amount_due_cents"]) != 5000 || page["currency"] != "USD" ||
		page["invoice_status"] != "finalized" || page["customer_name"] != cust["display_name"] {
		t.Errorf("payment-update page = %v; want invoice %s, 5000 USD due, finalized, for %v", page, invNumber, cust["display_name"])
	}
	f.must(http.StatusUnauthorized, http.MethodGet, "/v1/public/payment-updates/not-a-real-token", nil)

	// Checkout opens one Stripe setup session; the link then is spent.
	checkout := f.must(http.StatusCreated, http.MethodPost, "/v1/public/payment-updates/"+token+"/checkout", nil)
	if !strings.HasPrefix(str(checkout["url"]), "https://checkout.stripe.com/") {
		t.Errorf("checkout url = %v, want the Stripe session's url", checkout["url"])
	}
	f.must(http.StatusUnauthorized, http.MethodPost, "/v1/public/payment-updates/"+token+"/checkout", nil)
	f.must(http.StatusUnauthorized, http.MethodGet, "/v1/public/payment-updates/"+token, nil)
	if n := stripe.count("POST /v1/checkout/sessions"); n != 1 {
		t.Errorf("Stripe checkout sessions created: %d, want 1", n)
	}

	// The customer pays by bank transfer instead; the operator records it.
	note := []byte(`{"note":"Bank transfer, flow e2e"}`)
	idemKey := withHeader("Idempotency-Key", "flow-record-"+uniq())
	paid := f.send(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", note, admin, idemKey)
	if paid.status != http.StatusOK {
		t.Fatalf("record payment: status %d; body: %s", paid.status, paid.raw)
	}
	assertOfflinePaid(t, paid.body, 5000)

	// A retry with the same Idempotency-Key gets the same answer back and
	// runs nothing again.
	replay := f.send(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", note, admin, idemKey)
	if replay.status != http.StatusOK || replay.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(replay.raw, paid.raw) {
		t.Errorf("replay with the same key: status %d, Idempotent-Replayed %q, same body %v; want 200, true, true",
			replay.status, replay.header.Get("Idempotent-Replayed"), bytes.Equal(replay.raw, paid.raw))
	}
	// A new request is refused: the invoice is already paid.
	if again := f.must(http.StatusConflict, http.MethodPost, "/v1/invoices/"+invID+"/record-payment", note, admin); errCode(again) != "invalid_state" {
		t.Errorf("record a second payment: code %q, want invalid_state", errCode(again))
	}

	// The invoice page, the timeline and dunning all agree.
	assertOfflinePaid(t, f.invoice(invID), 5000)
	rows := f.timeline(invID, "invoice.paid")
	if len(rows) != 1 || rows[0]["description"] != "Payment received (offline)" || num(rows[0]["amount_cents"]) != 5000 {
		t.Errorf("timeline paid rows = %v; want one \"Payment received (offline)\" for 5000", rows)
	}
	runs = f.dunningRuns(invID)
	if len(runs) != 1 || runs[0]["state"] != "resolved" || runs[0]["resolution"] != "payment_recovered" {
		t.Errorf("dunning runs after the payment = %v; want the run resolved as payment_recovered", runs)
	}

	// Recorded once: one invoice.payment_recorded event, one audit row.
	if n := f.queuedEvents("invoice.payment_recorded", invID); n != 1 {
		t.Errorf("invoice.payment_recorded events queued: %d, want 1", n)
	}
	audit := f.must(http.StatusOK, http.MethodGet, "/v1/audit-log?resource_type=invoice&resource_id="+invID, nil, admin)
	recorded := 0
	for _, e := range arr(audit["data"]) {
		if obj(obj(e)["metadata"])["action"] == "payment_recorded" {
			recorded++
		}
	}
	if recorded != 1 {
		t.Errorf("payment_recorded audit rows: %d, want 1", recorded)
	}
}

func assertOfflinePaid(t *testing.T, inv map[string]any, cents int64) {
	t.Helper()
	if inv["status"] != "paid" || inv["payment_status"] != "succeeded" || num(inv["amount_due_cents"]) != 0 ||
		num(inv["amount_paid_cents"]) != cents || inv["paid_at"] == nil ||
		!strings.HasPrefix(str(inv["stripe_payment_intent_id"]), "out_of_band:") {
		t.Errorf("invoice after an offline payment = status %v, payment_status %v, due %v, paid %v, paid_at %v, payment %v; "+
			"want paid, succeeded, 0 due, %d paid, a paid_at, an out_of_band payment",
			inv["status"], inv["payment_status"], inv["amount_due_cents"], inv["amount_paid_cents"], inv["paid_at"],
			inv["stripe_payment_intent_id"], cents)
	}
}
