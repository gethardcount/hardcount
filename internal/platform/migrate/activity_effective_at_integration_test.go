package migrate

import (
	"context"
	"testing"
)

// activityEffectiveAtVersion is 00232_activity_lines_effective_at.sql; 00233
// fills the column and 00234 makes it NOT NULL.
const activityEffectiveAtVersion = 232

// 00232 to 00234 date each email and charge attempt at the instant its action
// decided. A row from before takes its test clock's instant, else the time it
// was written. The Down gives each test-clock row its sim_effective_at back
// (00237 drops the column), so the previous version's pair check lets the
// dispatcher update it; and Up runs again.
func TestActivityEffectiveAt_FillAndRollback(t *testing.T) {
	dsn, db := scratchDB(t, "hardcount_migrate_activity_effective_at")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, activityEffectiveAtVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", activityEffectiveAtVersion, err)
	}
	exec := func(q string) {
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
			t.Fatalf("%s: %v", oneLine(q), err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// Each row's id with one of its times, in minutes, or "-" for none.
	times := func(col string) string {
		t.Helper()
		var s string
		if err := db.QueryRow(`SELECT string_agg(id || '=' || COALESCE(to_char(at AT TIME ZONE 'UTC', 'MM-DD HH24:MI'), '-'), ' ' ORDER BY id)
			FROM (SELECT id, ` + col + ` AS at FROM email_outbox
				UNION ALL SELECT stripe_payment_intent_id, ` + col + ` FROM invoice_charge_attempts) r`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	exec(`INSERT INTO tenants (id, name) VALUES ('ten_eat', 'Effective at')`)
	exec(`INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_eat', 'ten_eat', 'eat', 'EAT')`)
	exec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, billing_period_start, billing_period_end, is_simulated)
		VALUES
		('inv_clk', 'ten_eat', 'cus_eat', 'EAT-1', 'finalized', 'failed', 'USD', 4900, 4900, 4900, 0, '2026-08-01', '2026-09-01', true),
		('inv_real', 'ten_eat', 'cus_eat', 'EAT-2', 'finalized', 'failed', 'USD', 4900, 4900, 4900, 0, '2026-10-01', '2026-11-01', false)`)
	exec(`INSERT INTO email_outbox (id, tenant_id, email_type, payload, created_at, sim_effective_at, test_clock_id)
		VALUES
		('em_clk', 'ten_eat', 'invoice', '{}', '2026-11-02T10:00:00Z', '2026-09-01T00:00:00Z', 'clk_eat'),
		('em_real', 'ten_eat', 'invoice', '{}', '2026-11-02T10:00:00Z', NULL, NULL)`)
	exec(`INSERT INTO invoice_charge_attempts (tenant_id, invoice_id, stripe_payment_intent_id, trigger_source, outcome, occurred_at, sim_effective_at)
		VALUES
		('ten_eat', 'inv_clk', 'pi_clk', 'auto_charge', 'failed', '2026-11-02T10:00:00Z', '2026-09-01T00:00:00Z'),
		('ten_eat', 'inv_real', 'pi_real', 'auto_charge', 'failed', '2026-11-02T10:00:00Z', NULL)`)

	// Up to before 00241, which deletes every row of a test clock.
	beforeClockDelete := filesBefore(t, deleteTestClocksVersion)
	if err := up(ctx, dsn, beforeClockDelete); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got, want := times("effective_at"), "em_clk=09-01 00:00 em_real=11-02 10:00 pi_clk=09-01 00:00 pi_real=11-02 10:00"; got != want {
		t.Errorf("effective_at after Up: %s, want %s", got, want)
	}
	var left string
	if err := db.QueryRow(`SELECT COALESCE(string_agg(table_name::text, ' ' ORDER BY table_name), '') FROM information_schema.columns
		WHERE column_name = 'sim_effective_at' AND table_name IN ('email_outbox', 'invoice_charge_attempts')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != "" {
		t.Errorf("sim_effective_at still on %v after Up (00237 drops it)", left)
	}

	// Rows as this version writes them on a test clock: effective_at, and no
	// sim_effective_at.
	exec(`INSERT INTO email_outbox (id, tenant_id, email_type, payload, effective_at, test_clock_id)
		VALUES ('em_new', 'ten_eat', 'invoice', '{}', '2026-09-05T00:00:00Z', 'clk_eat')`)
	exec(`INSERT INTO invoice_charge_attempts (tenant_id, invoice_id, stripe_payment_intent_id, trigger_source, outcome, occurred_at, effective_at)
		VALUES ('ten_eat', 'inv_clk', 'pi_new', 'auto_charge', 'failed', now(), '2026-09-05T00:00:00Z')`)

	if _, err := down(ctx, dsn, migrationFiles(), activityEffectiveAtVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", activityEffectiveAtVersion, err)
	}
	if got, want := times("sim_effective_at"), "em_clk=09-01 00:00 em_new=09-05 00:00 em_real=- pi_clk=09-01 00:00 pi_new=09-05 00:00 pi_real=-"; got != want {
		t.Errorf("sim_effective_at after Down: %s, want %s", got, want)
	}
	exec(`UPDATE email_outbox SET status = 'dispatched', dispatched_at = now() WHERE id = 'em_new'`)

	if err := up(ctx, dsn, beforeClockDelete); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}
