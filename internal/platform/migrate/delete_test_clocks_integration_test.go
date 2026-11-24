package migrate

import (
	"context"
	"testing"
)

// deleteTestClocksVersion is 00241_delete_test_clocks.sql. The test stops
// before 00242, which drops the clock tables and columns it reads.
const deleteTestClocksVersion = 241

// A clock with a customer on it, and a customer on no clock, each with an
// invoice and a line, and an email of each. Clocks are test mode only. The
// clock's email is reached by its clock id, not through a customer, so no
// foreign key would show it left behind.
const deleteTestClocksSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_tc', 'Clock removal');
SELECT set_config('app.livemode', 'off', true);
INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status) VALUES ('tclk_gone', 'ten_tc', 'gone', now(), 'ready');
INSERT INTO customers (id, tenant_id, external_id, display_name, test_clock_id) VALUES
	('cus_clock', 'ten_tc', 'on-clock', 'On a clock', 'tclk_gone'),
	('cus_real', 'ten_tc', 'real', 'Real', NULL);
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end) VALUES
	('inv_clock', 'ten_tc', 'cus_clock', 'TC-1', 'finalized', 'pending', 'USD', 100, 100, 100, now(), now()),
	('inv_real', 'ten_tc', 'cus_real', 'TC-2', 'finalized', 'pending', 'USD', 100, 100, 100, now(), now());
INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents) VALUES
	('li_clock', 'ten_tc', 'inv_clock', 'add_on', 'On a clock', 100, 100),
	('li_real', 'ten_tc', 'inv_real', 'add_on', 'Real', 100, 100);
INSERT INTO email_outbox (id, tenant_id, email_type, payload, effective_at, test_clock_id) VALUES
	('em_clock', 'ten_tc', 'invoice', '{}', now(), 'tclk_gone'),
	('em_real', 'ten_tc', 'invoice', '{}', now(), NULL);
`

// TestDeleteTestClocks_UpAndDown: 00241 deletes every test clock and the
// customers on one with what they own, and keeps a customer on no clock with
// its invoice. The Down changes nothing, and Up runs again.
func TestDeleteTestClocks_UpAndDown(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_test_delete_test_clocks")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, deleteTestClocksVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", deleteTestClocksVersion, err)
	}
	if err := deadContractExec(ctx, db, deleteTestClocksSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	throughDelete := filesBefore(t, dropTestClockSchemaVersion)
	if err := up(ctx, dsn, throughDelete); err != nil {
		t.Fatalf("Up: %v", err)
	}
	for _, c := range []struct {
		table, id string
		want      bool
	}{
		{"test_clocks", "tclk_gone", false},
		{"customers", "cus_clock", false},
		{"invoices", "inv_clock", false},
		{"invoice_line_items", "li_clock", false},
		{"email_outbox", "em_clock", false},
		{"customers", "cus_real", true},
		{"invoices", "inv_real", true},
		{"invoice_line_items", "li_real", true},
		{"email_outbox", "em_real", true},
	} {
		if got := noDraftScalar[bool](t, db, `SELECT EXISTS (SELECT 1 FROM `+c.table+` WHERE id = '`+c.id+`')`); got != c.want {
			t.Errorf("after Up, %s %s exists = %v, want %v", c.table, c.id, got, c.want)
		}
	}

	if _, err := down(ctx, dsn, throughDelete, deleteTestClocksVersion-1); err != nil {
		t.Fatalf("Down to %d: %v", deleteTestClocksVersion-1, err)
	}
	if err := up(ctx, dsn, throughDelete); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
