package migrate

import (
	"context"
	"testing"
)

// topupInvoicesVersion is 00243_topup_invoices.sql; 00244 validates its CHECKs.
const topupInvoicesVersion = 243

const topupInvoicesSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_tu', 'Top-up');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_tu', 'ten_tu', 'tu', 'TU');
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency, billing_reason,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_tu', 'ten_tu', 'cus_tu', 'TU-1', 'draft', 'pending', 'USD', 'manual', 9000, 9000, 9000, now(), now());
`

// TestTopupInvoices_UpAndDown: after 00243 and 00244 an invoice may be a
// 'topup', and only a top-up never issued (a draft, or a draft voided) may
// have no number; both CHECKs are validated, and the line and the tenant
// settings gain their validity in days. The Down puts back the billing_reason
// CHECK exactly as it was and the NOT NULL on invoice_number, and drops the
// two columns; Up runs again.
func TestTopupInvoices_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_topup_invoices")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, topupInvoicesVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", topupInvoicesVersion, err)
	}
	if err := deadContractExec(ctx, db, topupInvoicesSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const reasonDef = `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'invoices_billing_reason_check'`
	before := noDraftScalar[string](t, db, reasonDef)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, c := range []string{"invoices_billing_reason_check", "invoices_invoice_number_check"} {
		if !noDraftScalar[bool](t, db, `SELECT convalidated FROM pg_constraint WHERE conname = '`+c+`'`) {
			t.Errorf("after Up, %s is not validated", c)
		}
	}
	for _, col := range [][2]string{{"invoice_line_items", "credit_valid_days"}, {"tenant_settings", "topup_credit_valid_days"}} {
		if !columnExists(t, db, col[0], col[1]) {
			t.Errorf("after Up, %s has no %s", col[0], col[1])
		}
	}
	if err := deadContractExec(ctx, db, `UPDATE invoices SET billing_reason = 'topup', invoice_number = NULL WHERE id = 'inv_tu'`); err != nil {
		t.Fatalf("after Up, an unnumbered top-up draft: %v", err)
	}
	if err := deadContractExec(ctx, db, `UPDATE invoices SET status = 'voided' WHERE id = 'inv_tu'`); err != nil {
		t.Fatalf("after Up, an unnumbered top-up voided as a draft: %v", err)
	}
	for q, want := range map[string]string{
		`UPDATE invoices SET status = 'paid' WHERE id = 'inv_tu'`:                             "invoices_invoice_number_check",
		`UPDATE invoices SET billing_reason = 'manual', status = 'draft' WHERE id = 'inv_tu'`: "invoices_invoice_number_check",
		`UPDATE invoices SET billing_reason = 'gift' WHERE id = 'inv_tu'`:                     "invoices_billing_reason_check",
	} {
		if got := refusedBy(t, db, q); got != want {
			t.Errorf("after Up, %s: refused by %q, want %q", q, got, want)
		}
	}

	// The Down refuses a top-up, which the old CHECKs do not allow.
	if err := deadContractExec(ctx, db, `UPDATE invoices SET billing_reason = 'manual', invoice_number = 'TU-1', status = 'draft' WHERE id = 'inv_tu'`); err != nil {
		t.Fatalf("restore the manual invoice: %v", err)
	}
	if _, err := down(ctx, dsn, migrationFiles(), topupInvoicesVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", topupInvoicesVersion-1, err)
	}
	if after := noDraftScalar[string](t, db, reasonDef); after != before {
		t.Errorf("after the Down, the billing_reason CHECK is %q, want it as before: %q", after, before)
	}
	if got := noDraftScalar[string](t, db, `SELECT is_nullable FROM information_schema.columns WHERE table_name = 'invoices' AND column_name = 'invoice_number'`); got != "NO" {
		t.Errorf("after the Down, invoice_number is_nullable = %s, want NO", got)
	}
	if columnExists(t, db, "invoice_line_items", "credit_valid_days") || columnExists(t, db, "tenant_settings", "topup_credit_valid_days") {
		t.Error("after the Down, a validity column is still there")
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
