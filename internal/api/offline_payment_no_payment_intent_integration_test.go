package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// An offline payment is money that moved outside Velox (a bank transfer, a
// cheque). Velox used to record it by writing "out_of_band:<time>" into the
// invoice's Stripe PaymentIntent column, so every reader of that column had
// to know the prefix, and one that did not offered a card refund on a bank
// transfer (VLX-000068). Now the column holds only Stripe's ids: an offline
// payment leaves it empty and sets paid_out_of_band, Stripe's own field.
//
// Both operator paths that record one are driven over the real router and
// database (POST /v1/invoices/{id}/record-payment, and resolving a payment
// recovery run as payment_recovered). Then each reader that branches on how
// the invoice was paid is asked through its own endpoint.
func TestOfflinePayment_LeavesNoPaymentIntent_EveryReader(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Offline Payment Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}

	// A dunning policy, so finalizing an invoice with no card opens a run.
	call("POST", "/v1/dunning/policies", flowReq{body: map[string]any{
		"name": "Offline payment policy", "enabled": true, "retry_days": []int{3, 3},
	}}).want(t, 201, "create dunning policy")
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "offline-pay", "display_name": "Offline Payer", "email": "ap@offline-pay.test",
	}}).want(t, 201, "create customer").body, "id")

	const due = int64(5362)
	newInvoice := func(desc string) string {
		t.Helper()
		id := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": desc, "quantity": 1, "unit_amount_cents": due}},
		}}).want(t, 201, "create invoice: "+desc).body, "id")
		call("POST", "/v1/invoices/"+id+"/finalize", flowReq{}).want(t, 200, "finalize: "+desc)
		return id
	}
	runID := func(invoiceID string) string {
		t.Helper()
		runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+invoiceID, flowReq{}).want(t, 200, "list dunning runs").body, "data")
		if len(runs) != 1 {
			t.Fatalf("invoice %s has %d dunning runs, want 1", invoiceID, len(runs))
		}
		return flowStr(runs[0], "id")
	}
	// column reads what the database holds, past any reader's COALESCE.
	column := func(invoiceID string) (pi *string, attempts int) {
		t.Helper()
		ctx := postgres.WithLivemode(context.Background(), false)
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		defer postgres.Rollback(tx)
		if err := tx.QueryRowContext(ctx, `SELECT stripe_payment_intent_id FROM invoices WHERE id = $1`, invoiceID).Scan(&pi); err != nil {
			t.Fatalf("read stripe_payment_intent_id: %v", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM invoice_charge_attempts WHERE invoice_id = $1`, invoiceID).Scan(&attempts); err != nil {
			t.Fatalf("count charge attempts: %v", err)
		}
		return pi, attempts
	}

	byRecordPayment := newInvoice("Paid by bank transfer")
	call("POST", "/v1/invoices/"+byRecordPayment+"/record-payment", flowReq{body: map[string]any{"note": "Wire ref 1042"}}).
		want(t, 200, "record offline payment")

	byDunningResolve := newInvoice("Paid by cheque, recovery resolved")
	call("POST", "/v1/dunning/runs/"+runID(byDunningResolve)+"/resolve", flowReq{body: map[string]any{"resolution": "payment_recovered"}}).
		want(t, 200, "resolve recovery as payment_recovered")

	for _, c := range []struct {
		name, id, note string
	}{
		{"record-payment", byRecordPayment, "Wire ref 1042"},
		{"payment recovery resolved as payment_recovered", byDunningResolve, "Recovered via dunning resolution"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := call("GET", "/v1/invoices/"+c.id, flowReq{}).want(t, 200, "get invoice")
			inv := flowObj(resp.body, "invoice")

			// The invoice: paid in full, and nothing in the PaymentIntent field.
			if flowStr(inv, "status") != "paid" || flowStr(inv, "payment_status") != "succeeded" ||
				flowInt(inv, "amount_paid_cents") != due || flowInt(inv, "amount_due_cents") != 0 {
				t.Errorf("invoice = %s/%s paid %d due %d, want paid/succeeded paid %d due 0",
					flowStr(inv, "status"), flowStr(inv, "payment_status"), flowInt(inv, "amount_paid_cents"), flowInt(inv, "amount_due_cents"), due)
			}
			if pi, ok := inv["stripe_payment_intent_id"]; ok {
				t.Errorf("stripe_payment_intent_id = %v; an offline payment has no Stripe PaymentIntent", pi)
			}
			if got, _ := inv["paid_out_of_band"].(bool); !got {
				t.Errorf("paid_out_of_band = %v, want true", inv["paid_out_of_band"])
			}
			pi, attempts := column(c.id)
			if pi != nil {
				t.Errorf("invoices.stripe_payment_intent_id = %q, want NULL", *pi)
			}
			// ADR-103: a charge attempt is a charge sent to Stripe. An offline
			// payment sends none.
			if attempts != 0 {
				t.Errorf("charge attempt rows = %d, want 0", attempts)
			}

			// The timeline names it an offline payment, with the operator's note.
			var paidRows []map[string]any
			for _, e := range flowList(call("GET", "/v1/invoices/"+c.id+"/payment-timeline", flowReq{}).want(t, 200, "payment timeline").body, "events") {
				if flowStr(e, "event_type") == "invoice.paid" {
					paidRows = append(paidRows, e)
				}
			}
			if len(paidRows) != 1 || flowStr(paidRows[0], "description") != "Offline payment received" || flowInt(paidRows[0], "amount_cents") != due {
				t.Errorf("timeline paid rows = %v, want one \"Offline payment received\" row for %d", paidRows, due)
			}

			// No card to refund: a refund allocation is refused, and says why.
			refused := call("POST", "/v1/credit-notes", flowReq{body: map[string]any{
				"invoice_id": c.id, "reason": "Billing error", "auto_issue": true, "refund_amount_cents": 1000,
				"lines": []map[string]any{{"description": "Billing error", "quantity": 1, "unit_amount_cents": 1000}},
			}})
			if refused.status != 422 || !strings.Contains(refused.errMessage(), "not paid by card") {
				t.Errorf("credit note refunding to a card: %d %q, want 422 saying the invoice was not paid by card", refused.status, refused.errMessage())
			}
			direct := call("POST", "/v1/invoices/"+c.id+"/refund", flowReq{body: map[string]any{"amount_cents": 1000, "reason": "requested_by_customer"}})
			if direct.status != 409 || !strings.Contains(direct.errMessage(), "paid outside Stripe") {
				t.Errorf("direct refund: %d %q, want 409 saying the invoice was paid outside Stripe", direct.status, direct.errMessage())
			}
			// Money returned outside Velox is recorded as such.
			cn := call("POST", "/v1/credit-notes", flowReq{body: map[string]any{
				"invoice_id": c.id, "reason": "Billing error", "auto_issue": true, "out_of_band_amount_cents": 1000,
				"lines": []map[string]any{{"description": "Billing error", "quantity": 1, "unit_amount_cents": 1000}},
			}}).want(t, 201, "credit note returned outside Velox")
			if flowInt(cn.body, "refund_amount_cents") != 0 || flowInt(cn.body, "out_of_band_amount_cents") != 1000 {
				t.Errorf("credit note channels refund %d out_of_band %d, want 0/1000",
					flowInt(cn.body, "refund_amount_cents"), flowInt(cn.body, "out_of_band_amount_cents"))
			}
			if s := flowStr(cn.body, "refund_status"); s != "" && s != "none" {
				t.Errorf("credit note refund_status = %q, want none: no Stripe refund runs", s)
			}

			// The recovery run is resolved as payment_recovered.
			runs := flowList(call("GET", "/v1/dunning/runs?invoice_id="+c.id, flowReq{}).want(t, 200, "list dunning runs").body, "data")
			if len(runs) != 1 || flowStr(runs[0], "state") != "resolved" || flowStr(runs[0], "resolution") != "payment_recovered" {
				t.Errorf("dunning runs = %v, want one resolved as payment_recovered", runs)
			}
		})
	}
}
