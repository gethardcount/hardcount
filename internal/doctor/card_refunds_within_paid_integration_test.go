package doctor_test

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/doctor"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// card_refunds_exceed_paid lists the invoices whose card refunds add up to
// more than the card paid: the refunds of issued notes that did not fail,
// plus the refunds made in Stripe of the same payment that moved or may move
// money and that no note holds. The rows are written directly, because the
// flagged one is what the caps refuse to write.
func TestDoctor_CardRefundsExceedPaid(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	var check []doctor.Check
	for _, c := range doctor.Checks {
		if c.Name == "card_refunds_exceed_paid" {
			check = append(check, c)
		}
	}
	if len(check) != 1 {
		t.Fatal("check card_refunds_exceed_paid not found")
	}

	tenantID := testutil.CreateTestTenant(t, db, "Doctor Card Refunds")
	seed := func(q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("seed begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatalf("set livemode: %v", err)
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}
	seed(`INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ('cus_crdoc', $1, 'crdoc', 'CR Doc', '')`, tenantID)
	// paid makes an invoice paid 90.00 by card with payment pi_<id>.
	paid := func(id string) {
		seed(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
				subtotal_cents, total_amount_cents, amount_paid_cents, amount_due_cents, billing_period_start, billing_period_end,
				paid_at, stripe_payment_intent_id)
			VALUES ($1, $2, 'cus_crdoc', $1, 'paid', 'succeeded', 'USD', 9000, 9000, 9000, 0, now(), now(), now(), 'pi_' || $1)`,
			id, tenantID)
	}
	// note issues a credit note refunding cents to the card, in refund status
	// status, holding Stripe refund refundID when it is set.
	note := func(id, invoiceID string, cents int64, status, refundID string) {
		seed(`INSERT INTO credit_notes (id, tenant_id, invoice_id, customer_id, credit_note_number, status, reason,
				subtotal_cents, total_cents, refund_amount_cents, currency, refund_status, stripe_refund_id, issued_at)
			VALUES ($1, $2, $3, 'cus_crdoc', $1, 'issued', 'refund', $4, $4, $4, 'USD', $5, NULLIF($6, ''), now())`,
			id, tenantID, invoiceID, cents, status, refundID)
	}
	// stripeRefund records a refund made in Stripe of payment pi.
	stripeRefund := func(invoiceID, refundID, pi string, cents int64, status, outcome, creditNoteID string) {
		review := ""
		if outcome == "needs_review" {
			review = "prepaid_commit"
		}
		seed(`INSERT INTO invoice_stripe_refunds (tenant_id, invoice_id, stripe_refund_id, stripe_payment_intent_id,
				amount_cents, currency, status, outcome, review_reason, credit_note_id,
				refund_created_at, first_event_at, last_event_at, last_stripe_event_id)
			VALUES ($1, $2, $3, $4, $5, 'USD', $6, $7, $8, NULLIF($9, ''), now(), now(), now(), 'evt_' || $3)`,
			tenantID, invoiceID, refundID, pi, cents, status, outcome, review, creditNoteID)
	}

	// 50.00 by a note and 50.00 in Stripe on a 90.00 payment: over.
	paid("hc_inv_crdoc_over")
	note("hc_cn_crdoc_over", "hc_inv_crdoc_over", 5000, "succeeded", "")
	stripeRefund("hc_inv_crdoc_over", "re_crdoc_over", "pi_hc_inv_crdoc_over", 5000, "pending", "awaiting", "")
	// The same, with the refund made in Stripe failed: within.
	paid("hc_inv_crdoc_failed_stripe")
	note("hc_cn_crdoc_failed_stripe", "hc_inv_crdoc_failed_stripe", 5000, "succeeded", "")
	stripeRefund("hc_inv_crdoc_failed_stripe", "re_crdoc_failed_stripe", "pi_hc_inv_crdoc_failed_stripe", 5000, "failed", "not_refunded", "")
	// A note whose card refund failed, and a refund made in Stripe for it: within.
	paid("hc_inv_crdoc_failed_note")
	note("hc_cn_crdoc_failed_note", "hc_inv_crdoc_failed_note", 9000, "failed", "")
	stripeRefund("hc_inv_crdoc_failed_note", "re_crdoc_failed_note", "pi_hc_inv_crdoc_failed_note", 9000, "succeeded", "needs_review", "")
	// A refund made in Stripe that a note holds counts once: within.
	paid("hc_inv_crdoc_held")
	note("hc_cn_crdoc_held", "hc_inv_crdoc_held", 9000, "succeeded", "re_crdoc_held")
	stripeRefund("hc_inv_crdoc_held", "re_crdoc_held", "pi_hc_inv_crdoc_held", 9000, "succeeded", "credit_note", "hc_cn_crdoc_held")
	// A refund of another payment on the invoice: within.
	paid("hc_inv_crdoc_other")
	note("hc_cn_crdoc_other", "hc_inv_crdoc_other", 9000, "succeeded", "")
	stripeRefund("hc_inv_crdoc_other", "re_crdoc_other", "pi_crdoc_second_charge", 9000, "succeeded", "other_payment", "")

	res := doctor.Run(ctx, admin, check)
	for _, e := range res.Errors {
		t.Fatalf("check failed to run: %v", e)
	}
	var flagged []string
	for _, v := range res.Violations {
		flagged = append(flagged, v.RowID)
	}
	if !slices.Equal(flagged, []string{"hc_inv_crdoc_over"}) {
		t.Errorf("flagged %v, want only hc_inv_crdoc_over", flagged)
	}
}
