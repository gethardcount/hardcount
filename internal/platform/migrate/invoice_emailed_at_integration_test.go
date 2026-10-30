package migrate

import (
	"context"
	"database/sql"
	"testing"
)

// invoiceEmailedAtVersion is 00220_invoice_emailed_at.sql.
const invoiceEmailedAtVersion = 220

// 00220 adds invoices.invoice_emailed_at, the send-once marker for the
// invoice email the collector sends without Stripe (ADR-060 amendment
// 2026-10-30). An invoice from before has it NULL, so the collector may still
// email it once; the column takes a stamp; the Down removes it, and Up runs
// again.
func TestInvoiceEmailedAt_UpAndRollback(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_invoice_emailed_at")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, invoiceEmailedAtVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", invoiceEmailedAtVersion, err)
	}
	exec := func(q string) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true), set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(q); err != nil {
			t.Fatalf("%s: %v", oneLine(q), err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants (id, name) VALUES ('ten_iea', 'Emailed at')`)
	exec(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_iea', 'ten_iea', 'iea', 'IEA')`)
	exec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, billing_period_start, billing_period_end)
		VALUES ('inv_iea', 'ten_iea', 'cus_iea', 'IEA-1', 'finalized', 'pending', 'USD', 4900, 4900, 4900, 0, '2026-10-01', '2026-11-01')`)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	var at sql.NullTime
	if err := db.QueryRow(`SELECT invoice_emailed_at FROM invoices WHERE id = 'inv_iea'`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at.Valid {
		t.Errorf("an invoice from before the migration has invoice_emailed_at %v, want NULL", at.Time)
	}
	exec(`UPDATE invoices SET invoice_emailed_at = now() WHERE id = 'inv_iea'`)

	if _, err := down(ctx, dsn, migrationFiles(), invoiceEmailedAtVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", invoiceEmailedAtVersion, err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'invoices' AND column_name = 'invoice_emailed_at')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the Down left invoice_emailed_at")
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
