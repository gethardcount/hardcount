package migrate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// cycleSlotVersion is the migration that builds idx_invoices_billing_period_dedup,
// the period slot without manual invoices; the next one drops
// idx_invoices_billing_idempotency, the slot that counted them (#1331).
const cycleSlotVersion = 215

// cycleSlotSeed is a subscription whose period [Apr 1, May 1) is already
// held by a manual invoice, as the bug left it: a manual invoice created
// with the subscription and its exact period took the slot before the cycle
// close could.
const cycleSlotSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_cs', 'Cycle slot');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_cs', 'ten_cs', 'cs', 'CS');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status) VALUES ('sub_cs', 'ten_cs', 'cs', 'CS', 'cus_cs', 'active');
` + `INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end, billing_reason)
	VALUES ('inv_cs_manual', 'ten_cs', 'cus_cs', 'sub_cs', 'CS-1', 'finalized', 'pending', 'USD', 500, 500, 500,
		'2026-04-01T00:00:00Z', '2026-05-01T00:00:00Z', 'manual');`

// cycleSlotInvoice is an invoice of sub_cs over [Apr 1, May 1) with the
// given id and billing reason.
func cycleSlotInvoice(id, reason string) string {
	return `INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end, billing_reason)
	VALUES ('` + id + `', 'ten_cs', 'cus_cs', 'sub_cs', '` + id + `', 'finalized', 'pending', 'USD', 3000, 3000, 3000,
		'2026-04-01T00:00:00Z', '2026-05-01T00:00:00Z', '` + reason + `')`
}

// TestCyclePeriodSlot_ManualInvoicesDoNotHoldIt: after the up, a manual
// invoice no longer holds a subscription's period, so the cycle invoice for
// that period can be written; a second cycle invoice for it is still
// refused. The up runs on a database where a manual invoice already holds a
// period, and leaves that invoice as it was: the new index's rows are a
// subset of the old one's, so it cannot meet a duplicate.
//
// The down builds the old index back. It fails while a manual invoice
// shares a period with a cycle invoice, and works once one of them is
// voided. The up then runs again.
//
// The first up stops before oldDataVersion, which drops an invoice column
// the row snapshot reads.
//
// Mutation: an up that keeps manual invoices in the slot turns this red.
func TestCyclePeriodSlot_ManualInvoicesDoNotHoldIt(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_cycleslot_migrate")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, cycleSlotVersion)); err != nil {
		t.Fatalf("up to before %d: %v", cycleSlotVersion, err)
	}
	if err := deadContractExec(ctx, db, cycleSlotSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := deadContractExec(ctx, db, cycleSlotInvoice("inv_cs_cycle", "subscription_cycle")); !isUniqueViolation(err, "idx_invoices_billing_idempotency") {
		t.Fatalf("before the up, a cycle invoice beside the manual one: %v, want the old slot to refuse it", err)
	}
	before := cycleSlotManualRow(t, db)

	if err := up(ctx, dsn, filesBefore(t, oldDataVersion)); err != nil {
		t.Fatalf("up: %v", err)
	}
	wantSlotIndexes(t, db, "after the up", false, true)
	if after := cycleSlotManualRow(t, db); after != before {
		t.Errorf("the up changed the manual invoice:\nbefore %s\nafter  %s", before, after)
	}
	if err := deadContractExec(ctx, db, cycleSlotInvoice("inv_cs_cycle", "subscription_cycle")); err != nil {
		t.Fatalf("after the up, the cycle invoice beside the manual one: %v", err)
	}
	if err := deadContractExec(ctx, db, cycleSlotInvoice("inv_cs_cycle2", "subscription_cycle")); !isUniqueViolation(err, "idx_invoices_billing_period_dedup") {
		t.Errorf("after the up, a second cycle invoice for the period: %v, want idx_invoices_billing_period_dedup to refuse it", err)
	}
	if err := deadContractExec(ctx, db, cycleSlotInvoice("inv_cs_manual2", "manual")); err != nil {
		t.Errorf("after the up, a second manual invoice for the period: %v", err)
	}

	if _, err := down(ctx, dsn, migrationFiles(), cycleSlotVersion-1); err == nil {
		t.Fatal("down with a manual and a cycle invoice in one period: no error, want the old index's build to fail on them")
	}
	if err := deadContractExec(ctx, db, `UPDATE invoices SET status = 'voided', voided_at = now() WHERE id IN ('inv_cs_manual', 'inv_cs_manual2')`); err != nil {
		t.Fatalf("void the manual invoices: %v", err)
	}
	if _, err := down(ctx, dsn, migrationFiles(), cycleSlotVersion-1); err != nil {
		t.Fatalf("down to %d: %v", cycleSlotVersion-1, err)
	}
	wantSlotIndexes(t, db, "after the down", true, false)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("up again: %v", err)
	}
	wantSlotIndexes(t, db, "after the second up", false, true)
}

// wantSlotIndexes checks which of the two slot indexes exist, and that each
// one present is valid (a failed concurrent build leaves an INVALID index).
func wantSlotIndexes(t *testing.T, db *sql.DB, when string, wantOld, wantNew bool) {
	t.Helper()
	for name, want := range map[string]bool{"idx_invoices_billing_idempotency": wantOld, "idx_invoices_billing_period_dedup": wantNew} {
		var valid sql.NullBool
		err := db.QueryRow(`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`, name).Scan(&valid)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if want {
				t.Errorf("%s: %s is missing", when, name)
			}
		case err != nil:
			t.Fatalf("%s: read %s: %v", when, name, err)
		case !want:
			t.Errorf("%s: %s still exists", when, name)
		case !valid.Bool:
			t.Errorf("%s: %s is INVALID", when, name)
		}
	}
}

// cycleSlotManualRow is the seeded manual invoice as text.
func cycleSlotManualRow(t *testing.T, db *sql.DB) string {
	t.Helper()
	var s string
	if err := withBypass(context.Background(), db, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT row_to_json(i)::text FROM invoices i WHERE id = 'inv_cs_manual'`).Scan(&s)
	}); err != nil {
		t.Fatalf("read the manual invoice: %v", err)
	}
	return s
}

// isUniqueViolation reports whether err is a unique violation on index.
func isUniqueViolation(err error, index string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && strings.EqualFold(pg.ConstraintName, index)
}
