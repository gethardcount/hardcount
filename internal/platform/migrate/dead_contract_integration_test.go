package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// deadContractSeed is the parent rows every statement below hangs off, in
// the schema before 00206 (a dunning policy's schedule is its three old
// columns; one retry needs no retry_schedule entry; a meter has a key).
// deadContractSeedNow is the same rows in the schema after 00206 and 00208
// (retry_days; a meter has an event_name).
var (
	deadContractSeed    = fmt.Sprintf(deadContractSeedRows, "max_retry_attempts", "1", "key")
	deadContractSeedNow = fmt.Sprintf(deadContractSeedRows, "retry_days", "'[3]'", "event_name")
)

const deadContractSeedRows = `
INSERT INTO tenants (id, name) VALUES ('ten_dc', 'Dead contract');
INSERT INTO customers (id, tenant_id, external_id, display_name) VALUES ('cus_dc', 'ten_dc', 'dc', 'DC');
INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, status) VALUES ('pln_dc', 'ten_dc', 'dc', 'DC', 'USD', 'monthly', 'active');
INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status) VALUES ('sub_dc', 'ten_dc', 'dc', 'DC', 'cus_dc', 'active');
INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
		subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
	VALUES ('inv_dc', 'ten_dc', 'cus_dc', 'DC-1', 'finalized', 'pending', 'USD', 1000, 1000, 1000, now() - interval '30 days', now());
INSERT INTO dunning_policies (id, tenant_id, name, %s) VALUES ('dpol_dc', 'ten_dc', 'DC', %s);
INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state)
	VALUES ('drun_dc', 'ten_dc', 'inv_dc', 'cus_dc', 'dpol_dc', 'active');
INSERT INTO meters (id, tenant_id, %s, name) VALUES ('mtr_dc', 'ten_dc', 'dc', 'DC');
`

// TestDeadContract_RefusesRowsHoldingRemovedValues: 00201 removes values
// and a table that no Velox version ever wrote. A row that holds one was
// written outside Velox, so the upgrade stops before changing anything (a
// draft plan stays draft too), the error names every kind of row it found
// and how many, and its hint agrees with the wrapper's "fix the cause and
// run the upgrade again": change or delete those rows, with a runbook
// section that exists. Once those rows are gone, it runs.
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
	exec(`UPDATE invoices SET tax_status = 'failed' WHERE id = 'inv_dc'`)
	exec(`UPDATE invoice_dunning_runs SET state = 'paused' WHERE id = 'drun_dc'`)
	exec(`INSERT INTO invoice_dunning_events (id, run_id, tenant_id, invoice_id, event_type, state) VALUES
		('devt_sched', 'drun_dc', 'ten_dc', 'inv_dc', 'retry_scheduled', 'paused')`)
	exec(`INSERT INTO billed_entries (tenant_id, customer_id, meter_id, amount_cents) VALUES ('ten_dc', 'cus_dc', 'mtr_dc', 5)`)

	err := up(ctx, dsn, migrationFiles())
	if err == nil {
		t.Fatal("Up ran over rows holding removed values; it must stop and name them")
	}
	t.Logf("the refusal: %v", err)
	for _, want := range []string{
		"1 subscription(s) with status archived",
		"1 invoice(s) with tax_status failed",
		"1 payment recovery run(s) with state paused",
		"1 payment recovery event(s) with event_type retry_scheduled, retry_succeeded, retry_failed, paused or resumed",
		"1 payment recovery event(s) with state paused",
		"1 row(s) in billed_entries",
		"No Velox version wrote these values",
		"Change or delete those rows, then run the upgrade again",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	// A draft plan, a discount or tax line, a manually_resolved run, an
	// imported invoice's stripe_invoice_id and coupon rows are history an
	// earlier version wrote: none is refused. The hint must not offer an
	// option the wrapper's own text contradicts.
	for _, notWant := range []string{"plan(s)", "invoice line(s)", "manually_resolved", "stripe_invoice_id", "coupon",
		"Keep running the release you have", "new database"} {
		if strings.Contains(err.Error(), notWant) {
			t.Errorf("the refusal says %q:\n%v", notWant, err)
		}
	}
	assertRunbookSectionExists(t, err.Error())
	if !tableExists(t, db, "public.billed_entries") {
		t.Fatal("the refused Up dropped billed_entries")
	}
	var planStatus string
	if err := db.QueryRow(`SELECT status FROM plans WHERE id = 'pln_draft'`).Scan(&planStatus); err != nil || planStatus != "draft" {
		t.Fatalf("the refused Up changed a plan: %q, %v", planStatus, err)
	}

	exec(`DELETE FROM invoice_dunning_events WHERE id = 'devt_sched'`)
	exec(`UPDATE invoice_dunning_runs SET state = 'active' WHERE id = 'drun_dc'`)
	exec(`UPDATE invoices SET tax_status = 'pending' WHERE id = 'inv_dc'`)
	exec(`DELETE FROM subscriptions WHERE id = 'sub_arch'`)
	exec(`DELETE FROM billed_entries`)
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up with no rows holding removed values: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM plans WHERE id = 'pln_draft'`).Scan(&planStatus); err != nil || planStatus != "active" {
		t.Fatalf("after Up the draft plan is %q (%v), want active", planStatus, err)
	}
}

// TestDeadContract_KeepsIssuedInvoiceLinesAndActivatesDraftPlans: until
// #1324 the API added discount and tax lines, and until this release a plan
// PATCH could set status draft. 00201 must upgrade such a database without
// touching an issued invoice: a paid invoice's discount line and a
// finalized invoice's tax line stay as they were, so its lines still add up
// to its subtotal. A draft plan was sold and billed like an active one, so
// it becomes active.
func TestDeadContract_KeepsIssuedInvoiceLinesAndActivatesDraftPlans(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_deadc_b_migrate_keep")
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
	exec(`INSERT INTO plans (id, tenant_id, code, name, currency, billing_interval, status) VALUES ('pln_draft', 'ten_dc', 'draft', 'D', 'USD', 'monthly', 'draft')`)
	exec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, paid_at, billing_period_start, billing_period_end)
		VALUES ('inv_paid', 'ten_dc', 'cus_dc', 'DC-2', 'paid', 'succeeded', 'USD', 900, 900, 0, 900, now(), now() - interval '30 days', now())`)
	exec(`INSERT INTO invoice_line_items (id, tenant_id, invoice_id, line_type, description, amount_cents, total_amount_cents) VALUES
		('li_base', 'ten_dc', 'inv_paid', 'base_fee', 'Base', 1000, 1000),
		('li_disc', 'ten_dc', 'inv_paid', 'discount', 'Discount', -100, -100),
		('li_tax', 'ten_dc', 'inv_dc', 'tax', 'Tax', 0, 0)`)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up over a paid invoice's discount line, a tax line and a draft plan: %v", err)
	}

	rows, err := db.Query(`SELECT id, line_type, amount_cents FROM invoice_line_items WHERE id IN ('li_base', 'li_disc', 'li_tax') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var id, lt string
		var amt int64
		if err := rows.Scan(&id, &lt, &amt); err != nil {
			t.Fatal(err)
		}
		got[id] = lt + "/" + strconv.FormatInt(amt, 10)
	}
	_ = rows.Close()
	want := map[string]string{"li_base": "base_fee/1000", "li_disc": "discount/-100", "li_tax": "tax/0"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("after Up line %s is %q, want %q (an issued invoice's lines are not changed)", id, got[id], w)
		}
	}
	var lineSum, subtotal int64
	if err := db.QueryRow(`SELECT (SELECT COALESCE(sum(amount_cents), 0) FROM invoice_line_items WHERE invoice_id = 'inv_paid'), subtotal_cents
		FROM invoices WHERE id = 'inv_paid'`).Scan(&lineSum, &subtotal); err != nil {
		t.Fatal(err)
	}
	if lineSum != subtotal {
		t.Errorf("after Up the paid invoice's lines add up to %d against a subtotal of %d", lineSum, subtotal)
	}
	var planStatus string
	if err := db.QueryRow(`SELECT status FROM plans WHERE id = 'pln_draft'`).Scan(&planStatus); err != nil || planStatus != "active" {
		t.Errorf("after Up the draft plan is %q (%v), want active", planStatus, err)
	}
}

// TestDeadContract_KeepsHistoryEarlierVersionsWrote: three kinds of row an
// earlier Velox wrote must survive 00201 unchanged, because there is no
// true value to rewrite them to and deleting them deletes history:
//   - a run v0.1.0 closed with resolution manually_resolved while its
//     invoice stayed finalized (the NIM-000243 shape migration 0170 kept on
//     purpose);
//   - an invoice the Stripe importer (removed 2026-04-29) wrote with its
//     stripe_invoice_id;
//   - coupon, redemption and customer discount rows from before coupons
//     were cut (ADR-039 kept the tables for them).
//
// The upgrade runs, and every row reads as it did.
func TestDeadContract_KeepsHistoryEarlierVersionsWrote(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_deadc_b_migrate_history")
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
	exec(`UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'manually_resolved', resolved_at = now() WHERE id = 'drun_dc'`)
	exec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, paid_at, billing_period_start, billing_period_end, stripe_invoice_id)
		VALUES ('inv_imp', 'ten_dc', 'cus_dc', 'IMP-1', 'paid', 'succeeded', 'USD', 500, 500, 0, 500, now(), now() - interval '30 days', now(), 'in_imported')`)
	exec(`INSERT INTO coupons (id, tenant_id, code) VALUES ('cpn_dc', 'ten_dc', 'DC')`)
	exec(`INSERT INTO coupon_redemptions (id, tenant_id, coupon_id, customer_id, invoice_id, discount_cents) VALUES ('crd_dc', 'ten_dc', 'cpn_dc', 'cus_dc', 'inv_dc', 100)`)
	exec(`INSERT INTO customer_discounts (tenant_id, customer_id, coupon_id) VALUES ('ten_dc', 'cus_dc', 'cpn_dc')`)

	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up over rows an earlier version wrote: %v", err)
	}

	var state, resolution, invStatus string
	if err := db.QueryRow(`SELECT r.state, r.resolution, i.status FROM invoice_dunning_runs r JOIN invoices i ON i.id = r.invoice_id
		WHERE r.id = 'drun_dc'`).Scan(&state, &resolution, &invStatus); err != nil {
		t.Fatal(err)
	}
	if state != "resolved" || resolution != "manually_resolved" || invStatus != "finalized" {
		t.Errorf("after Up the legacy run reads %s/%s on a %s invoice, want resolved/manually_resolved on finalized", state, resolution, invStatus)
	}
	var stripeID string
	if err := db.QueryRow(`SELECT stripe_invoice_id FROM invoices WHERE id = 'inv_imp'`).Scan(&stripeID); err != nil || stripeID != "in_imported" {
		t.Errorf("after Up the imported invoice's stripe_invoice_id is %q (%v), want in_imported", stripeID, err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM coupons WHERE id = 'cpn_dc'`,
		`SELECT count(*) FROM coupon_redemptions WHERE id = 'crd_dc' AND discount_cents = 100`,
		`SELECT count(*) FROM customer_discounts WHERE coupon_id = 'cpn_dc'`,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != 1 {
			t.Errorf("after Up %s = %d (%v), want 1", q, n, err)
		}
	}
}

// TestDeadContract_UpNarrowsAndDownRestores: after 00201 and 00202 every
// removed value is refused by its CHECK and every kept value still goes in;
// billed_entries is gone, and the coupon tables and
// invoices.stripe_invoice_id stay; a plan is active, a threshold does not
// reset the cycle unless a writer says so, and a payment recovery run has
// no default state the CHECK refuses. The Down puts the old schema back,
// and Up runs again.
func TestDeadContract_UpNarrowsAndDownRestores(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_deadc_b_migrate_round")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if err := deadContractExec(ctx, db, deadContractSeedNow); err != nil {
		t.Fatalf("seed: %v", err)
	}

	type stmt struct{ name, sql string }
	removed := []stmt{
		{"subscription archived", `UPDATE subscriptions SET status = 'archived' WHERE id = 'sub_dc'`},
		{"plan draft", `UPDATE plans SET status = 'draft' WHERE id = 'pln_dc'`},
		{"tax_status failed", `UPDATE invoices SET tax_status = 'failed' WHERE id = 'inv_dc'`},
		{"run state paused", `UPDATE invoice_dunning_runs SET state = 'paused' WHERE id = 'drun_dc'`},
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
		// Lines added before #1324 keep these types; the API refuses them
		// on a new line.
		{"line types discount and tax", `INSERT INTO invoice_line_items (tenant_id, invoice_id, line_type, description) VALUES
			('ten_dc', 'inv_dc', 'discount', 'd'), ('ten_dc', 'inv_dc', 'tax', 't')`},
		{"events", `INSERT INTO invoice_dunning_events (run_id, tenant_id, invoice_id, event_type, state) VALUES
			('drun_dc', 'ten_dc', 'inv_dc', 'dunning_started', 'active'), ('drun_dc', 'ten_dc', 'inv_dc', 'retry_attempted', 'active'),
			('drun_dc', 'ten_dc', 'inv_dc', 'escalated', 'escalated'), ('drun_dc', 'ten_dc', 'inv_dc', 'resolved', 'resolved')`},
		{"run states and resolutions", `UPDATE invoice_dunning_runs SET state = 'escalated', resolution = 'retries_exhausted' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'invoice_voided' WHERE id = 'drun_dc';
			UPDATE invoice_dunning_runs SET state = 'active', resolution = 'action_failed' WHERE id = 'drun_dc'`},
		// v0.1.0 wrote it; migration 0170 kept it for the runs it could not
		// map. No writer emits it now.
		{"legacy resolution manually_resolved", `UPDATE invoice_dunning_runs SET state = 'resolved', resolution = 'manually_resolved', resolved_at = now() WHERE id = 'drun_dc'`},
		{"plan and subscription defaults", `INSERT INTO plans (id, tenant_id, code, name) VALUES ('pln_def', 'ten_dc', 'def', 'Def');
			INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id) VALUES ('sub_def', 'ten_dc', 'def', 'Def', 'cus_dc')`},
	}
	for _, s := range kept {
		if err := deadContractExec(ctx, db, s.sql); err != nil {
			t.Errorf("%s was refused after 00201: %v", s.name, err)
		}
	}
	var planStatus string
	if err := db.QueryRow(`SELECT status FROM plans WHERE id = 'pln_def'`).Scan(&planStatus); err != nil || planStatus != "active" {
		t.Errorf("a plan inserted without a status is %q (%v), want active", planStatus, err)
	}
	// 00201's false default for billing_threshold_reset_cycle is checked by
	// TestThresholdResetColumnDropped: 00205 drops the column, and its Down
	// puts it back as 00201 left it.

	if tableExists(t, db, "public.billed_entries") {
		t.Error("00201 left the table billed_entries")
	}
	for _, table := range []string{"coupons", "coupon_redemptions", "customer_discounts"} {
		if !tableExists(t, db, "public."+table) {
			t.Errorf("00201 dropped the table %s, which holds rows from before coupons were cut", table)
		}
	}
	if !columnExists(t, db, "invoices", "stripe_invoice_id") || !indexExists(t, db, "idx_invoices_stripe_invoice_id") {
		t.Error("00201 dropped invoices.stripe_invoice_id or its unique index, which imported invoices hold")
	}
	if d := columnDefault(t, db, "invoice_dunning_runs", "state"); d != "" {
		t.Errorf("after 00201 invoice_dunning_runs.state defaults to %s; every writer names the state, and a default must not be a value the CHECK refuses", d)
	}
	if err := deadContractExec(ctx, db, `INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id)
		VALUES ('drun_nostate', 'ten_dc', 'inv_dc', 'cus_dc', 'dpol_dc')`); err == nil || !strings.Contains(err.Error(), "23502") {
		t.Errorf("a run inserted without a state: %v, want a not-null error (23502)", err)
	}
	for _, c := range []string{
		"subscriptions_status_check", "plans_status_check", "invoices_tax_status_check",
		"invoice_dunning_runs_resolution_check",
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
	if !tableExists(t, db, "public.billed_entries") {
		t.Error("the Down did not put back the table billed_entries")
	} else {
		var rls, force bool
		if err := db.QueryRow(`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'public.billed_entries'::regclass`).Scan(&rls, &force); err != nil || !rls || !force {
			t.Errorf("the Down put back billed_entries without forced row-level security (%v, %v, %v)", rls, force, err)
		}
	}
	if d := columnDefault(t, db, "invoice_dunning_runs", "state"); d != "'scheduled'::text" {
		t.Errorf("after the Down, invoice_dunning_runs.state defaults to %q, want 'scheduled'::text", d)
	}
	// Each old value goes in again. Each statement runs in a transaction
	// that is rolled back, so the next Up finds no such row.
	restored := append([]stmt{}, removed...)
	restored = append(restored,
		stmt{"billed entry", `INSERT INTO billed_entries (tenant_id, customer_id, meter_id, amount_cents) VALUES ('ten_dc', 'cus_dc', 'mtr_dc', 5)`},
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

// columnDefault returns the column's default expression, or "" when it has
// none.
func columnDefault(t *testing.T, db *sql.DB, table, column string) string {
	t.Helper()
	var d sql.NullString
	if err := db.QueryRow(`SELECT column_default FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d.String
}

// assertRunbookSectionExists finds the runbook link with an anchor in msg
// and fails unless the runbook has a heading with that anchor.
func assertRunbookSectionExists(t *testing.T, msg string) {
	t.Helper()
	m := regexp.MustCompile(regexp.QuoteMeta(RunbookPath) + `#([a-z0-9-]+)`).FindStringSubmatch(msg)
	if m == nil {
		t.Errorf("the message names no section of %s:\n%s", RunbookPath, msg)
		return
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "..", RunbookPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		if githubAnchor(strings.TrimLeft(line, "# ")) == m[1] {
			return
		}
	}
	t.Errorf("%s has no heading with the anchor #%s", RunbookPath, m[1])
}

// githubAnchor is the anchor GitHub gives a Markdown heading: lower case,
// spaces to hyphens, other punctuation dropped.
func githubAnchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
	}
	return b.String()
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
