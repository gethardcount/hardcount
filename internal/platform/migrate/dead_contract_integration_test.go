package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// deadContractVersion is 00201_remove_dead_contract.sql; 00202 validates
// the CHECKs it narrows or adds.
const deadContractVersion = 201

// deadContractExec runs one statement with row-level security bypassed, in
// its own transaction, and returns its error.
func deadContractExec(ctx context.Context, db *sql.DB, q string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
		return err
	}
	if _, err := tx.Exec(q); err != nil {
		return err
	}
	return tx.Commit()
}

// deadContractSeed is the parent rows every statement below hangs off.
const deadContractSeed = `
INSERT INTO tenants (id, name) VALUES ('ten_dc', 'Dead contract');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_dc', 'ten_dc', 'dc', 'DC');
INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, status) VALUES ('pln_dc', 'ten_dc', 'dc', 'DC', 'USD', 'monthly', 'active');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status) VALUES ('sub_dc', 'ten_dc', 'dc', 'DC', 'cus_dc', 'active');
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_dc', 'ten_dc', 'cus_dc', 'DC-1', 'finalized', 'pending', 'USD', 1000, 1000, 1000, now() - interval '30 days', now());
INSERT INTO dunning_policies (id, tenant_id, name) VALUES ('dpol_dc', 'ten_dc', 'DC');
INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state)
	VALUES ('drun_dc', 'ten_dc', 'inv_dc', 'cus_dc', 'dpol_dc', 'active');
INSERT INTO meters (id, tenant_id, key, name) VALUES ('mtr_dc', 'ten_dc', 'dc', 'DC');
`

// TestDeadContract_RefusesRowsHoldingRemovedValues: 00201 removes values,
// columns and tables that no Velox code writes. It never rewrites an
// invoice or anything else to make room: when a row still holds a removed
// value, the upgrade stops before changing anything, and the error names
// every kind of row it found and how many. Once those rows are gone, it runs.
func TestDeadContract_RefusesRowsHoldingRemovedValues(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_deadc_b_migrate_refuse")
	ctx := context.Background()
	if err := up(ctx, dsn, filesBefore(t, deadContractVersion)); err != nil {
		t.Fatalf("Up to before %d: %v", deadContractVersion, err)
	}
	exec := func(q string) {
		t.Helper()
		if err := deadContractExec(ctx, db, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(deadContractSeed)
	exec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status) VALUES ('sub_arch', 'ten_dc', 'arch', 'A', 'cus_dc', 'archived')`)
	exec(`INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, status) VALUES ('pln_draft', 'ten_dc', 'draft', 'D', 'USD', 'monthly', 'draft')`)
	exec(`UPDATE invoices SET tax_status = 'failed', stripe_invoice_id = 'in_dc' WHERE id = 'inv_dc'`)
	exec(`INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description) VALUES
		('li_disc', 'ten_dc', 'inv_dc', 'discount', 'd'), ('li_tax', 'ten_dc', 'inv_dc', 'tax', 't')`)
	exec(`UPDATE invoice_dunning_runs SET state = 'paused', resolution = 'manually_resolved' WHERE id = 'drun_dc'`)
	exec(`INSERT INTO invoice_dunning_events (id, run_id, tenant_id, invoice_id, event_type, state) VALUES
		('devt_sched', 'drun_dc', 'ten_dc', 'inv_dc', 'retry_scheduled', 'paused')`)
	exec(`INSERT INTO coupons (id, tenant_id, code) VALUES ('cpn_dc', 'ten_dc', 'DC')`)
	exec(`INSERT INTO coupon_redemptions (id, tenant_id, coupon_id, customer_id) VALUES ('crd_dc', 'ten_dc', 'cpn_dc', 'cus_dc')`)
	exec(`INSERT INTO customer_discounts (tenant_id, customer_id, coupon_id) VALUES ('ten_dc', 'cus_dc', 'cpn_dc')`)
	exec(`INSERT INTO billed_entries (tenant_id, customer_id, meter_id, amount_cents) VALUES ('ten_dc', 'cus_dc', 'mtr_dc', 5)`)

	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("Up ran over rows holding removed values; it must stop and name them")
	}
	t.Logf("the refusal: %v", err)
	for _, want := range []string{
		"1 subscription(s) with status archived",
		"1 plan(s) with status draft",
		"1 invoice(s) with tax_status failed",
		"1 invoice(s) with a stripe_invoice_id",
		"2 invoice line(s) with line_type discount or tax",
		"1 payment recovery run(s) with state paused",
		"1 payment recovery run(s) with resolution manually_resolved",
		"1 payment recovery event(s) with event_type retry_scheduled, retry_succeeded, retry_failed, paused or resumed",
		"1 payment recovery event(s) with state paused",
		"1 row(s) in coupons",
		"1 row(s) in coupon_redemptions",
		"1 row(s) in customer_discounts",
		"1 row(s) in billed_entries",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if !tableExists(t, db, "public.coupons") {
		t.Fatal("the refused Up dropped coupons")
	}
	var desc string
	if err := db.QueryRow(`SELECT line_type || '/' || description FROM invoice_line_items WHERE id = 'li_disc'`).Scan(&desc); err != nil || desc != "discount/d" {
		t.Fatalf("the refused Up changed an invoice line: %q, %v", desc, err)
	}

	exec(`DELETE FROM invoice_dunning_events WHERE id = 'devt_sched'`)
	exec(`UPDATE invoice_dunning_runs SET state = 'active', resolution = NULL WHERE id = 'drun_dc'`)
	exec(`DELETE FROM invoice_line_items WHERE id IN ('li_disc', 'li_tax')`)
	exec(`UPDATE invoices SET tax_status = 'pending', stripe_invoice_id = NULL WHERE id = 'inv_dc'`)
	exec(`DELETE FROM subscriptions WHERE id = 'sub_arch'`)
	exec(`DELETE FROM plans WHERE id = 'pln_draft'`)
	exec(`DELETE FROM billed_entries`)
	exec(`DELETE FROM customer_discounts`)
	exec(`DELETE FROM coupon_redemptions`)
	exec(`DELETE FROM coupons`)
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up with no rows holding removed values: %v", err)
	}
}

// TestDeadContract_UpNarrowsAndDownRestores: after 00201 and 00202 every
// removed value is refused by its CHECK and every kept value still goes in;
// the four tables and invoices.stripe_invoice_id are gone; a plan is active
// and a threshold does not reset the cycle unless a writer says so. The
// Down puts the old schema back, and Up runs again.
func TestDeadContract_UpNarrowsAndDownRestores(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_deadc_b_migrate_round")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := deadContractExec(ctx, db, deadContractSeed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	type stmt struct{ name, sql string }
	removed := []stmt{
		{"subscription archived", `UPDATE subscriptions SET status = 'archived' WHERE id = 'sub_dc'`},
		{"plan draft", `UPDATE plans SET status = 'draft' WHERE id = 'pln_dc'`},
		{"tax_status failed", `UPDATE invoices SET tax_status = 'failed' WHERE id = 'inv_dc'`},
		{"line_type discount", `INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description) VALUES ('ten_dc', 'inv_dc', 'discount', 'd')`},
		{"line_type tax", `INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description) VALUES ('ten_dc', 'inv_dc', 'tax', 't')`},
		{"run state paused", `UPDATE invoice_dunning_runs SET state = 'paused' WHERE id = 'drun_dc'`},
		{"run resolution manually_resolved", `UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'manually_resolved', resolved_at = now() WHERE id = 'drun_dc'`},
		{"event state paused", `INSERT INTO invoice_dunning_events (run_id, tenant_id, invoice_id, event_type, state) VALUES ('drun_dc', 'ten_dc', 'inv_dc', 'resolved', 'paused')`},
	}
	for _, et := range []string{"retry_scheduled", "retry_succeeded", "retry_failed", "paused", "resumed"} {
		removed = append(removed, stmt{"event_type " + et,
			`INSERT INTO invoice_dunning_events (run_id, tenant_id, invoice_id, event_type, state) VALUES ('drun_dc', 'ten_dc', 'inv_dc', '` + et + `', 'active')`})
	}
	for _, s := range removed {
		if err := deadContractExec(ctx, db, s.sql); err == nil {
			t.Errorf("%s was accepted after 00201", s.name)
		}
	}

	kept := []stmt{
		{"subscription statuses", `UPDATE subscriptions SET status = 'draft' WHERE id = 'sub_dc'; UPDATE subscriptions SET status = 'trialing' WHERE id = 'sub_dc';
			UPDATE subscriptions SET status = 'canceled' WHERE id = 'sub_dc'; UPDATE subscriptions SET status = 'active' WHERE id = 'sub_dc'`},
		{"plan archived", `UPDATE plans SET status = 'archived' WHERE id = 'pln_dc'`},
		{"tax_status ok and pending", `UPDATE invoices SET tax_status = 'pending' WHERE id = 'inv_dc'; UPDATE invoices SET tax_status = 'ok' WHERE id = 'inv_dc'`},
		{"line types", `INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description) VALUES
			('ten_dc', 'inv_dc', 'base_fee', 'b'), ('ten_dc', 'inv_dc', 'usage', 'u'), ('ten_dc', 'inv_dc', 'add_on', 'a')`},
		{"events", `INSERT INTO invoice_dunning_events (run_id, tenant_id, invoice_id, event_type, state) VALUES
			('drun_dc', 'ten_dc', 'inv_dc', 'dunning_started', 'active'), ('drun_dc', 'ten_dc', 'inv_dc', 'retry_attempted', 'active'),
			('drun_dc', 'ten_dc', 'inv_dc', 'escalated', 'escalated'), ('drun_dc', 'ten_dc', 'inv_dc', 'resolved', 'resolved')`},
		{"run states and resolutions", `UPDATE invoice_dunning_runs SET state = 'escalated', resolution = 'retries_exhausted' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'invoice_voided' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'active', resolution = 'action_failed' WHERE id = 'drun_dc'`},
		{"plan and subscription defaults", `INSERT INTO plans (id, tenant_id, code, name) VALUES ('pln_def', 'ten_dc', 'def', 'Def');
			INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id) VALUES ('sub_def', 'ten_dc', 'def', 'Def', 'cus_dc')`},
	}
	for _, s := range kept {
		if err := deadContractExec(ctx, db, s.sql); err != nil {
			t.Errorf("%s was refused after 00201: %v", s.name, err)
		}
	}
	var planStatus string
	var resetCycle bool
	if err := db.QueryRow(`SELECT status FROM plans WHERE id = 'pln_def'`).Scan(&planStatus); err != nil || planStatus != "active" {
		t.Errorf("a plan inserted without a status is %q (%v), want active", planStatus, err)
	}
	if err := db.QueryRow(`SELECT billing_threshold_reset_cycle FROM subscriptions WHERE id = 'sub_def'`).Scan(&resetCycle); err != nil || resetCycle {
		t.Errorf("a subscription inserted without the flag has billing_threshold_reset_cycle %v (%v), want false (the API default)", resetCycle, err)
	}

	for _, table := range []string{"coupons", "coupon_redemptions", "customer_discounts", "billed_entries"} {
		if tableExists(t, db, "public."+table) {
			t.Errorf("00201 left the table %s", table)
		}
	}
	if columnExists(t, db, "invoices", "stripe_invoice_id") {
		t.Error("00201 left invoices.stripe_invoice_id")
	}
	if indexExists(t, db, "idx_invoices_stripe_invoice_id") {
		t.Error("00201 left idx_invoices_stripe_invoice_id")
	}
	for _, c := range []string{
		"subscriptions_status_check", "plans_status_check", "invoices_tax_status_check",
		"invoice_line_items_line_type_check", "invoice_dunning_runs_resolution_check",
		"invoice_dunning_runs_state_check", "invoice_dunning_events_event_type_check", "invoice_dunning_events_state_check",
	} {
		var validated bool
		if err := db.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conname = $1`, c).Scan(&validated); err != nil {
			t.Errorf("read %s: %v", c, err)
			continue
		}
		if !validated {
			t.Errorf("00202 left %s not validated", c)
		}
	}

	if _, err := down(ctx, dsn, migrationFiles(), deadContractVersion-1); err != nil {
		t.Fatalf("Down to before %d: %v", deadContractVersion, err)
	}
	for _, table := range []string{"coupons", "coupon_redemptions", "customer_discounts", "billed_entries"} {
		if !tableExists(t, db, "public."+table) {
			t.Errorf("the Down did not put back the table %s", table)
		}
		var rls, force bool
		if err := db.QueryRow(`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, "public."+table).Scan(&rls, &force); err != nil || !rls || !force {
			t.Errorf("the Down put back %s without forced row-level security (%v, %v, %v)", table, rls, force, err)
		}
	}
	if !columnExists(t, db, "invoices", "stripe_invoice_id") || !indexExists(t, db, "idx_invoices_stripe_invoice_id") {
		t.Error("the Down did not put back invoices.stripe_invoice_id and its unique index")
	}
	// Each old value goes in again. Each statement runs in a transaction
	// that is rolled back, so the next Up finds no such row.
	restored := append([]stmt{}, removed...)
	restored = append(restored,
		stmt{"coupon chain", `INSERT INTO coupons (id, tenant_id, code) VALUES ('cpn_r', 'ten_dc', 'R');
			INSERT INTO coupon_redemptions (id, tenant_id, coupon_id, customer_id) VALUES ('crd_r', 'ten_dc', 'cpn_r', 'cus_dc');
			INSERT INTO customer_discounts (tenant_id, customer_id, coupon_id) VALUES ('ten_dc', 'cus_dc', 'cpn_r');
			INSERT INTO billed_entries (tenant_id, customer_id, meter_id, amount_cents) VALUES ('ten_dc', 'cus_dc', 'mtr_dc', 5)`},
	)
	for _, s := range restored {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`SELECT set_config('app.bypass_rls', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(s.sql); err != nil {
			t.Errorf("after the Down, %s was refused: %v", s.name, err)
		}
		_ = tx.Rollback()
	}
	var planDefault, resetDefault string
	if err := db.QueryRow(`SELECT pg_get_expr(adbin, adrelid) FROM pg_attrdef WHERE adrelid = 'plans'::regclass
		AND adnum = (SELECT attnum FROM pg_attribute WHERE attrelid = 'plans'::regclass AND attname = 'status')`).Scan(&planDefault); err != nil || !strings.Contains(planDefault, "draft") {
		t.Errorf("after the Down, plans.status defaults to %q (%v), want 'draft'", planDefault, err)
	}
	if err := db.QueryRow(`SELECT pg_get_expr(adbin, adrelid) FROM pg_attrdef WHERE adrelid = 'subscriptions'::regclass
		AND adnum = (SELECT attnum FROM pg_attribute WHERE attrelid = 'subscriptions'::regclass AND attname = 'billing_threshold_reset_cycle')`).Scan(&resetDefault); err != nil || resetDefault != "true" {
		t.Errorf("after the Down, billing_threshold_reset_cycle defaults to %q (%v), want true", resetDefault, err)
	}

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up again: %v", err)
	}
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`, table, column).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var ok bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}
