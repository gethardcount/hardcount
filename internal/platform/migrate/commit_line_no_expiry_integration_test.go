package migrate

import (
	"context"
	"testing"
)

// commitNoExpiryVersion is 00239_commit_line_no_expiry.sql; 00240 validates
// the CHECK it replaces.
const commitNoExpiryVersion = 239

const commitNoExpirySeed = `
INSERT INTO tenants (id, name) VALUES ('ten_cx', 'Commit expiry');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_cx', 'ten_cx', 'cx', 'CX');
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_cx', 'ten_cx', 'cus_cx', 'CX-1', 'draft', 'pending', 'USD', 9000, 9000, 9000, now(), now());
INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents,
		commit_granted_cents, commit_expires_at) VALUES
	('li_commit', 'ten_cx', 'inv_cx', 'add_on', 'Commit', 9000, 9000, 10000, now() + interval '30 days'),
	('li_setup', 'ten_cx', 'inv_cx', 'add_on', 'Setup', 1000, 1000, NULL, NULL);
`

// TestCommitLineNoExpiry_UpAndDown: 00239 drops commit_expires_at and keeps
// every commit line's granted amount; after 00240 the commit CHECK is
// validated and still refuses a commit on a line that is not add_on. The Down
// puts the column (empty) and the old CHECK back, and Up runs again.
func TestCommitLineNoExpiry_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_commit_no_expiry")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, commitNoExpiryVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", commitNoExpiryVersion, err)
	}
	if err := deadContractExec(ctx, db, commitNoExpirySeed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if columnExists(t, db, "invoice_line_items", "commit_expires_at") {
		t.Error("after Up, invoice_line_items still has commit_expires_at")
	}
	if got := noDraftScalar[int64](t, db, `SELECT commit_granted_cents FROM invoice_line_items WHERE id = 'li_commit'`); got != 10000 {
		t.Errorf("after Up, the commit line grants %d, want 10000", got)
	}
	if !noDraftScalar[bool](t, db, `SELECT convalidated FROM pg_constraint WHERE conname = 'invoice_line_items_commit_check'`) {
		t.Error("after Up, invoice_line_items_commit_check is not validated")
	}
	const commitOnUsage = `UPDATE invoice_line_items SET line_type = 'usage' WHERE id = 'li_commit'`
	if got := refusedBy(t, db, commitOnUsage); got != "invoice_line_items_commit_check" {
		t.Errorf("after Up, a commit on a usage line: refused by %q, want invoice_line_items_commit_check", got)
	}

	if _, err := down(ctx, dsn, migrationFiles(), commitNoExpiryVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", commitNoExpiryVersion-1, err)
	}
	const expiryOnUsage = `UPDATE invoice_line_items SET line_type = 'usage', commit_expires_at = now() WHERE id = 'li_setup'`
	if got := refusedBy(t, db, expiryOnUsage); got != "invoice_line_items_commit_check" {
		t.Errorf("after the Down, an expiry on a usage line: refused by %q, want invoice_line_items_commit_check", got)
	}
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
