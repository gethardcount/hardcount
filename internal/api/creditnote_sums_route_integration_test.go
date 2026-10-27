package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/config"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Two routes read what an invoice's credit notes add up to: a refund with
// no amount refunds what is left, and a written-off invoice owed a credit
// that was never applied carries a warning. Both counted only the first page
// of 50 notes. Each case puts 101 notes on one invoice, one past the largest
// page the store list returns (100), over the real router and database.
func TestCreditNoteSums_Routes_CountEveryNote(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil,
		WithRateLimits(config.RateLimits{GeneralPerMinute: 10_000, IngestPerSecond: 1000})))
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Credit Note Sums Co")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, req flowReq) flowResp {
		t.Helper()
		req.bearer = key
		return flowDo(t, ts, method, path, req)
	}
	customerID := flowStr(call("POST", "/v1/customers", flowReq{body: map[string]any{
		"external_id": "cn-sums", "display_name": "Sums Customer",
	}}).want(t, 201, "create customer").body, "id")
	creditNote := func(invoiceID string, cents int64, body map[string]any) flowResp {
		t.Helper()
		body["invoice_id"] = invoiceID
		body["reason"] = "Billing error"
		body["lines"] = []map[string]any{{"description": "Billing error", "quantity": 1, "unit_amount_cents": cents}}
		return call("POST", "/v1/credit-notes", flowReq{body: body}).want(t, 201, "create credit note")
	}

	t.Run("refund with no amount refunds what is left", func(t *testing.T) {
		// Paid by card: the store writes the paid invoice and its
		// PaymentIntent, as a card payment would leave them.
		ctx := postgres.WithLivemode(context.Background(), false)
		now := time.Now().UTC()
		inv, err := invoice.NewPostgresStore(db).Create(ctx, tenantID, domain.Invoice{
			CustomerID: customerID, InvoiceNumber: "INV-CNSUMS-PAID",
			Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
			SubtotalCents: 10000, TotalAmountCents: 10000, AmountPaidCents: 10000,
			BillingPeriodStart: now.Add(-720 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create paid invoice: %v", err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET stripe_payment_intent_id = 'pi_cnsums' WHERE id = $1`, inv.ID); err != nil {
			t.Fatalf("stamp payment intent: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}

		// 101 notes, each refunding 0.01 to the card. No Stripe account is
		// connected, so each refund fails at Stripe, and each note still
		// holds its refund amount against the cap.
		for range 101 {
			creditNote(inv.ID, 1, map[string]any{"auto_issue": true, "refund_amount_cents": 1})
		}

		resp := call("POST", "/v1/invoices/"+inv.ID+"/refund", flowReq{body: map[string]any{"reason": "requested_by_customer"}}).
			want(t, 200, "refund with no amount")
		if got := flowInt(resp.body, "refund_amount_cents"); got != 10000-101 {
			t.Errorf("refund_amount_cents = %d, want %d: 100.00 paid less the 1.01 already on refund notes", got, 10000-101)
		}
	})

	t.Run("written-off invoice owed a credit that was never applied warns", func(t *testing.T) {
		invoiceID := flowStr(call("POST", "/v1/invoices", flowReq{body: map[string]any{
			"customer_id": customerID,
			"line_items":  []map[string]any{{"description": "Seats", "quantity": 1, "unit_amount_cents": 10000}},
		}}).want(t, 201, "create invoice").body, "id")
		call("POST", "/v1/invoices/"+invoiceID+"/finalize", flowReq{}).want(t, 200, "finalize")

		// The oldest note is a draft voided before it issued: 1.00 the
		// invoice was owed and never got. Then 100 issued notes of 0.01.
		draft := flowStr(creditNote(invoiceID, 100, map[string]any{}).body, "id")
		call("POST", "/v1/credit-notes/"+draft+"/void", flowReq{}).want(t, 200, "void the draft")
		for range 100 {
			creditNote(invoiceID, 1, map[string]any{"auto_issue": true})
		}
		call("POST", "/v1/invoices/"+invoiceID+"/mark-uncollectible", flowReq{}).want(t, 200, "write off")

		inv := flowObj(call("GET", "/v1/invoices/"+invoiceID, flowReq{}).want(t, 200, "get invoice").body, "invoice")
		if code := flowStr(inv, "recovery_warning", "code"); code != "relief_not_reissued" {
			t.Errorf("recovery_warning code = %q, want relief_not_reissued: the voided draft's 1.00 was never applied", code)
		}
	})
}
