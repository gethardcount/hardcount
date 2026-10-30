package migrate

import (
	"context"
	"testing"
)

// invoiceStripeRefundsVersion is 00222_invoice_stripe_refunds.sql.
const invoiceStripeRefundsVersion = 222

// 00222 adds invoice_stripe_refunds, the record of refunds made in Stripe
// outside Hardcount (ADR-134), and credit_notes.recorded_from_stripe. A
// credit note from before reads false; the table forces row-level security;
// its CHECKs tie the credit note to the credit_note outcome and the review
// reason to the flagged ones; the Down removes both, and Up runs again.
func TestInvoiceStripeRefunds_UpAndRollback(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_invoice_stripe_refunds")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, invoiceStripeRefundsVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", invoiceStripeRefundsVersion, err)
	}
	exec := func(q string) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', 'off', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(q); err != nil {
			return err
		}
		return tx.Commit()
	}
	must := func(q string) {
		t.Helper()
		if err := exec(q); err != nil {
			t.Fatalf("%s: %v", oneLine(q), err)
		}
	}
	must(`INSERT INTO tenants (id, name) VALUES ('ten_isr', 'Stripe refunds')`)
	must(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_isr', 'ten_isr', 'isr', 'ISR')`)
	must(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, billing_period_start, billing_period_end)
		VALUES ('inv_isr', 'ten_isr', 'cus_isr', 'ISR-1', 'paid', 'succeeded', 'USD', 4900, 4900, 0, 4900, '2026-10-01', '2026-11-01')`)
	must(`INSERT INTO credit_notes (id, tenant_id, invoice_id, customer_id, credit_note_number, status, reason, total_cents, refund_amount_cents, currency)
		VALUES ('cn_isr', 'ten_isr', 'inv_isr', 'cus_isr', 'CN-ISR-1', 'issued', 'before', 4900, 4900, 'USD')`)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	var recorded bool
	if err := db.QueryRow(`SELECT recorded_from_stripe FROM credit_notes WHERE id = 'cn_isr'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded {
		t.Error("a credit note from before the migration reads recorded_from_stripe true, want false")
	}
	var rls, force bool
	if err := db.QueryRow(`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'invoice_stripe_refunds'`).Scan(&rls, &force); err != nil {
		t.Fatal(err)
	}
	if !rls || !force {
		t.Errorf("invoice_stripe_refunds row-level security enabled=%v forced=%v, want both", rls, force)
	}

	row := func(outcome, reason, creditNote string) string {
		return `INSERT INTO invoice_stripe_refunds (tenant_id, invoice_id, stripe_refund_id, stripe_payment_intent_id,
				amount_cents, currency, status, outcome, review_reason, credit_note_id, refund_created_at, last_event_at, last_stripe_event_id)
			VALUES ('ten_isr', 'inv_isr', 're_` + outcome + reason + `', 'pi_isr', 4900, 'USD', 'succeeded', '` + outcome + `', '` + reason + `', ` +
			creditNote + `, now(), now(), 'evt_isr')`
	}
	must(row("credit_note", "", "'cn_isr'"))
	must(row("needs_review", "seen_late", "NULL"))
	must(row("other_payment", "", "NULL"))
	for _, bad := range []struct{ name, q string }{
		{"credit_note without a credit note", row("credit_note", "x", "NULL")},
		{"needs_review without a reason", row("needs_review", "", "NULL")},
		{"other_payment with a reason", row("other_payment", "seen_late", "NULL")},
		{"a reason the CHECK does not list", row("needs_review", "because", "NULL")},
		{"dismissed without a dismissal time", row("dismissed", "seen_late", "NULL")},
		{"lower-case currency", `INSERT INTO invoice_stripe_refunds (tenant_id, invoice_id, stripe_refund_id, stripe_payment_intent_id,
				amount_cents, currency, status, outcome, refund_created_at, last_event_at, last_stripe_event_id)
			VALUES ('ten_isr', 'inv_isr', 're_lower', 'pi_isr', 4900, 'usd', 'succeeded', 'other_payment', now(), now(), 'evt_isr')`},
		{"a second row for one refund", row("other_payment", "", "NULL")},
	} {
		if err := exec(bad.q); err == nil {
			t.Errorf("%s: the insert succeeded, want a constraint error", bad.name)
		}
	}

	if _, err := down(ctx, dsn, migrationFiles(), invoiceStripeRefundsVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", invoiceStripeRefundsVersion, err)
	}
	var table, column bool
	if err := db.QueryRow(`SELECT to_regclass('public.invoice_stripe_refunds') IS NOT NULL,
			EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'credit_notes' AND column_name = 'recorded_from_stripe')`).Scan(&table, &column); err != nil {
		t.Fatal(err)
	}
	if table || column {
		t.Errorf("the Down left the table (%v) or the column (%v)", table, column)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
