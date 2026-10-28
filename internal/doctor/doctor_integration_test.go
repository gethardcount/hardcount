package doctor_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/gethardcount/hardcount/internal/doctor"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestDoctor_CleanSchemaIsQuiet_AndSeededViolationsAreCaught is the doctor's
// own positive control, in both directions.
//
// Direction 1 (quiet): every check must RUN against the real schema (a check
// that errors is schema drift — the doctor's own rot alarm) and a fresh
// database must produce ZERO violations, because the exactness rule says a
// check that flags legal states does not ship.
//
// Direction 2 (loud): a seeded violation must be caught by NAME. Without
// this arm, a doctor whose SQL silently matched nothing would look
// permanently healthy — the mirror image of crying wolf.
func TestDoctor_CleanSchemaIsQuiet_AndSeededViolationsAreCaught(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The doctor MUST run as a role that sees every row (the table owner /
	// an admin DSN). This test originally ran it on the RLS-scoped app pool
	// and the doctor reported a spotless bill of health over rows it could
	// not see — the exact silent-blindness failure this comment now warns
	// about. cmd/hardcount-doctor documents the same requirement.
	adminURL := strings.TrimSpace(os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if adminURL == "" {
		adminURL = "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// Direction 1 — all checks run, zero violations on a fresh DB.
	res := doctor.Run(ctx, admin, doctor.Checks)
	for _, e := range res.Errors {
		t.Errorf("check failed to run (schema drift?): %v", e)
	}
	for _, v := range res.Violations {
		t.Errorf("fresh database flagged by %s: row %s (%s) — either the seed data is illegal or the check is not exact",
			v.Check.Name, v.RowID, v.Detail)
	}

	// Direction 2 — seed two violations from different domains, raw SQL on
	// purpose: the states must be unreachable through any writer.
	tenantID := testutil.CreateTestTenant(t, db, "Doctor Loud")
	mustExec := func(q string, args ...any) {
		t.Helper()
		// TxBypass: the seeds are deliberately-illegal states no writer can
		// produce; RLS on the pool would (correctly) refuse them.
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("seed begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatalf("set livemode: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email)
		VALUES ('cus_doc1', $1, 'doc1', 'Doc', '')`, tenantID)
	// paid/succeeded with amount_due != 0 and paid_at NULL — violates both
	// invoice_paid_settled_coherence and invoice_due_conservation.
	mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status,
			currency, subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
			billing_period_start, billing_period_end)
		VALUES ('hc_inv_doc1', $1, 'cus_doc1', 'DOC-1', 'paid', 'succeeded', 'USD', 4000, 4000, 4000, 4000, 0,
			now() - interval '30 days', now())`, tenantID)
	// detached card still default — the charge-a-removed-card hazard.
	mustExec(`INSERT INTO payment_methods (id, tenant_id, livemode, customer_id, stripe_payment_method_id,
			type, is_default, detached_at, created_at, updated_at)
		VALUES ('hc_pm_doc1', $1, false, 'cus_doc1', 'pm_doc1', 'card', true, now(), now(), now())`, tenantID)

	// ADR-115 read-side alarm: the same usage instant of one (customer,
	// meter, price) on two live invoices. Each seed has its own
	// meter, so the seeds do not pair with each other. Five seeds:
	//   doc_dbl — threshold [P0,t1) + cycle [P0,P1) usage lines, SAME price
	//             → the double charge the period CAS exists to prevent: flagged.
	//   doc_res — threshold [P0,t1) + cycle residual [t1,P1), same price
	//             → half-open windows touch, do not overlap: NOT flagged.
	//   doc_max — threshold line of one price [P0,t1) + cycle line of another [P0,P1) on
	//             ONE meter (two prices bill different events): NOT flagged.
	//   doc_xa/doc_xb — two subscriptions of the customer, the second
	//             recreated the same day on the meter: A's final [P0,t1) and
	//             B's cycle [P0,P1) overlap → flagged (usage is aggregated per
	//             customer and meter, so both bill the same events).
	//   doc_ya/doc_yb — the same pair with B starting where A ended
	//             [t1,P1): NOT flagged.
	//   doc_za/doc_zb — the doc_x pair on plans whose prices differ, so the
	//             two lines carry different prices → still flagged (the
	//             other-price exemption is one subscription's).
	p0, t1, p1 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	seedSub := func(id string) {
		mustExec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
				current_billing_period_start, current_billing_period_end, next_billing_at)
			VALUES ($1, $2, $1, $1, 'cus_doc1', 'active', 'anniversary', $3, $4, $4)`, id, tenantID, p1, p1.AddDate(0, 1, 0))
	}
	seedInvoice := func(id, subID, reason string, start, end time.Time) {
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, subscription_id, invoice_number, status, payment_status,
				currency, billing_reason, billing_period_start, billing_period_end)
			VALUES ($1, $2, 'cus_doc1', $3, $1, 'finalized', 'pending', 'USD', $4, $5, $6)`, id, tenantID, subID, reason, start, end)
	}
	seedUsageLine := func(invID, meterID, priceID string, start, end time.Time) {
		mustExec(`INSERT INTO invoice_line_items (invoice_id, tenant_id, line_type, meter_id, price_id, description,
				billing_period_start, billing_period_end)
			VALUES ($1, $2, 'usage', $3, $4, 'seed', $5, $6)`, invID, tenantID, meterID, priceID, start, end)
	}
	seedSub("hc_sub_doc_dbl")
	seedInvoice("hc_inv_doc_dbl_t", "hc_sub_doc_dbl", "threshold", p0, t1)
	seedInvoice("hc_inv_doc_dbl_c", "hc_sub_doc_dbl", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_dbl_t", "mtr_doc_dbl", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_dbl_c", "mtr_doc_dbl", "prc_doc_a", p0, p1)
	seedSub("hc_sub_doc_res")
	seedInvoice("hc_inv_doc_res_t", "hc_sub_doc_res", "threshold", p0, t1)
	seedInvoice("hc_inv_doc_res_c", "hc_sub_doc_res", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_res_t", "mtr_doc_res", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_res_c", "mtr_doc_res", "prc_doc_a", t1, p1)
	seedSub("hc_sub_doc_max")
	seedInvoice("hc_inv_doc_max_t", "hc_sub_doc_max", "threshold", p0, t1)
	seedInvoice("hc_inv_doc_max_c", "hc_sub_doc_max", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_max_t", "mtr_doc_max", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_max_c", "mtr_doc_max", "prc_doc_max", p0, p1)
	seedSub("hc_sub_doc_xa")
	seedSub("hc_sub_doc_xb")
	seedInvoice("hc_inv_doc_xa_f", "hc_sub_doc_xa", "subscription_cancel", p0, t1)
	seedInvoice("hc_inv_doc_xb_c", "hc_sub_doc_xb", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_xa_f", "mtr_doc_x", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_xb_c", "mtr_doc_x", "prc_doc_a", p0, p1)
	seedSub("hc_sub_doc_ya")
	seedSub("hc_sub_doc_yb")
	seedInvoice("hc_inv_doc_ya_f", "hc_sub_doc_ya", "subscription_cancel", p0, t1)
	seedInvoice("hc_inv_doc_yb_c", "hc_sub_doc_yb", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_ya_f", "mtr_doc_y", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_yb_c", "mtr_doc_y", "prc_doc_a", t1, p1)
	seedSub("hc_sub_doc_za")
	seedSub("hc_sub_doc_zb")
	seedInvoice("hc_inv_doc_za_f", "hc_sub_doc_za", "subscription_cancel", p0, t1)
	seedInvoice("hc_inv_doc_zb_c", "hc_sub_doc_zb", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_za_f", "mtr_doc_z", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_zb_c", "mtr_doc_z", "prc_doc_b", p0, p1)
	// A written-off threshold invoice keeps its window billed (ADR-066
	// amendment 2026-10-23), so a cycle invoice billing the same window
	// again is the double charge: flagged (doc_wo). A voided one billed
	// nothing, so the cycle invoice re-billing its window is correct: NOT
	// flagged (doc_vd).
	seedSub("hc_sub_doc_wo")
	seedInvoice("hc_inv_doc_wo_t", "hc_sub_doc_wo", "threshold", p0, t1)
	seedInvoice("hc_inv_doc_wo_c", "hc_sub_doc_wo", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_wo_t", "mtr_doc_wo", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_wo_c", "mtr_doc_wo", "prc_doc_a", p0, p1)
	mustExec(`UPDATE invoices SET status = 'uncollectible', uncollectible_at = now() WHERE id = 'hc_inv_doc_wo_t'`)
	seedSub("hc_sub_doc_vd")
	seedInvoice("hc_inv_doc_vd_t", "hc_sub_doc_vd", "threshold", p0, t1)
	seedInvoice("hc_inv_doc_vd_c", "hc_sub_doc_vd", "subscription_cycle", p0, p1)
	seedUsageLine("hc_inv_doc_vd_t", "mtr_doc_vd", "prc_doc_a", p0, t1)
	seedUsageLine("hc_inv_doc_vd_c", "mtr_doc_vd", "prc_doc_a", p0, p1)
	mustExec(`UPDATE invoices SET status = 'voided', voided_at = now() WHERE id = 'hc_inv_doc_vd_t'`)
	// A manual invoice that names the subscription and its period is not the
	// cycle invoice (#1331): the pair is legal, so
	// cycle_invoice_unique_per_subscription_period stays quiet (doc_man).
	seedSub("hc_sub_doc_man")
	seedInvoice("hc_inv_doc_man_c", "hc_sub_doc_man", "subscription_cycle", p0, p1)
	seedInvoice("hc_inv_doc_man_m", "hc_sub_doc_man", "manual", p0, p1)

	// SB-1: an active run with no next action is invisible to both due-run
	// pickers forever — flagged. Control: an escalated run with no next action
	// is the legal terminal shape — NOT flagged by the new check.
	mustExec(`INSERT INTO dunning_policies (id, tenant_id, name, retry_days) VALUES ('hc_dpol_doc', $1, 'doc', '[3]')`, tenantID)
	mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, attempt_count, last_attempt_at)
		VALUES ('hc_drun_doc_strand', $1, 'hc_inv_doc1', 'cus_doc1', 'hc_dpol_doc', 'active', 3, now())`, tenantID)
	mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, attempt_count, last_attempt_at, resolved_at, resolution)
		VALUES ('hc_drun_doc_escal', $1, 'hc_inv_doc_max_c', 'cus_doc1', 'hc_dpol_doc', 'escalated', 3, now(), now(), 'retries_exhausted')`, tenantID)

	// invoice_due_conservation with credit notes. One defect, three controls:
	//   doc_cn_over  — 100.00 invoice, 60.00 credits applied, an issued 100.00
	//                  note that reduced it: relieved by 160.00. The old clamp
	//                  left amount_due at 0, so only the over-total arm sees it.
	//   doc_cn_split — the same story after the fix: the note returned the
	//                  credits (credits_applied 0). NOT flagged.
	//   doc_cn_paid  — a 40.00 note reduced it, then 60.00 was paid. NOT flagged.
	//   doc_cn_bal   — paid in full, then a 30.00 note credited to the balance
	//                  (its credit channel carries the total). NOT flagged.
	seedCNInvoice := func(id, status, payment string, due, paid, applied int64) {
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status,
				currency, subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
				paid_at, billing_period_start, billing_period_end)
			VALUES ($1, $2, 'cus_doc1', $1, $3, $4, 'USD', 10000, 10000, $5, $6, $7,
				CASE WHEN $3 = 'paid' THEN now() END, now() - interval '30 days', now())`,
			id, tenantID, status, payment, due, paid, applied)
	}
	seedIssuedNote := func(id, invoiceID string, total, creditChannel int64) {
		mustExec(`INSERT INTO credit_notes (id, tenant_id, invoice_id, customer_id, credit_note_number, reason, status,
				subtotal_cents, total_cents, credit_amount_cents, currency)
			VALUES ($1, $2, $3, 'cus_doc1', $1, 'order_change', 'issued', $4, $4, $5, 'USD')`,
			id, tenantID, invoiceID, total, creditChannel)
	}
	seedCNInvoice("hc_inv_doc_cn_over", "finalized", "pending", 0, 0, 6000)
	seedIssuedNote("hc_cn_doc_over", "hc_inv_doc_cn_over", 10000, 0)
	seedCNInvoice("hc_inv_doc_cn_split", "finalized", "pending", 0, 0, 0)
	seedIssuedNote("hc_cn_doc_split", "hc_inv_doc_cn_split", 10000, 0)
	seedCNInvoice("hc_inv_doc_cn_paid", "paid", "succeeded", 0, 6000, 0)
	seedIssuedNote("hc_cn_doc_paid", "hc_inv_doc_cn_paid", 4000, 0)
	seedCNInvoice("hc_inv_doc_cn_bal", "paid", "succeeded", 0, 10000, 0)
	seedIssuedNote("hc_cn_doc_bal", "hc_inv_doc_cn_bal", 3000, 3000)

	res = doctor.Run(ctx, admin, doctor.Checks)
	overRelief := false
	for _, v := range res.Violations {
		if v.Check.Name != "invoice_due_conservation" {
			continue
		}
		switch {
		case strings.Contains(v.RowID, "doc_cn_over"):
			overRelief = true
		case strings.Contains(v.RowID, "doc_cn_"):
			t.Errorf("false positive: a correctly accounted invoice with a credit note is flagged: %s %s", v.RowID, v.Detail)
		}
	}
	if !overRelief {
		t.Error("an invoice relieved by more than its total (credits applied + a credit note) is NOT caught by invoice_due_conservation")
	}
	want := map[string]bool{
		"invoice_paid_settled_coherence":              false,
		"payment_methods_detached_but_still_default":  false,
		"usage_billed_once_per_customer_meter_window": false,
		"dunning_active_run_without_next_action":      false,
	}
	crossSub, crossSubOtherBucket, writtenOff := false, false, false
	for _, v := range res.Violations {
		if v.Check.Name == "cycle_invoice_unique_per_subscription_period" && strings.Contains(v.RowID+v.Detail, "doc_man") {
			t.Errorf("false positive: a manual invoice beside the cycle invoice of the same period is flagged: %s %s", v.RowID, v.Detail)
		}
		if _, ok := want[v.Check.Name]; ok && strings.Contains(v.RowID+v.Detail, "doc1") {
			want[v.Check.Name] = true
		}
		if v.Check.Name == "dunning_active_run_without_next_action" {
			switch {
			case strings.Contains(v.RowID, "doc_strand"):
				want[v.Check.Name] = true
			case strings.Contains(v.RowID, "doc_escal"):
				t.Errorf("false positive: an escalated run with no next action is the legal terminal shape: %s", v.RowID)
			}
		}
		if v.Check.Name == "usage_billed_once_per_customer_meter_window" {
			switch {
			case strings.Contains(v.RowID, "doc_dbl"):
				want[v.Check.Name] = true
			case strings.Contains(v.RowID, "doc_xa"):
				crossSub = true
			case strings.Contains(v.RowID, "doc_za"):
				crossSubOtherBucket = true
			case strings.Contains(v.RowID, "doc_wo"):
				writtenOff = true
			case strings.Contains(v.RowID+v.Detail, "doc_vd"):
				t.Errorf("false positive: a cycle invoice re-billing a VOIDED threshold invoice's window is flagged — a void billed nothing: %s %s", v.RowID, v.Detail)
			case strings.Contains(v.RowID+v.Detail, "doc_y"):
				t.Errorf("false positive: a recreated subscription that starts where its predecessor ended is flagged: %s %s", v.RowID, v.Detail)
			case strings.Contains(v.RowID+v.Detail, "doc_res"):
				t.Errorf("false positive: a threshold window beside its residual is flagged — the compare must be half-open: %s %s", v.RowID, v.Detail)
			case strings.Contains(v.RowID+v.Detail, "doc_max"):
				t.Errorf("false positive: two prices of one meter are flagged — pair per price, not per meter: %s %s", v.RowID, v.Detail)
			}
		}
	}
	if !crossSub {
		t.Error("two subscriptions of one customer billing the same usage instant of a meter are NOT caught by usage_billed_once_per_customer_meter_window: the check pairs invoices of one subscription only")
	}
	if !writtenOff {
		t.Error("a cycle invoice billing a WRITTEN-OFF threshold invoice's window again is NOT caught by usage_billed_once_per_customer_meter_window: a write-off keeps its window billed")
	}
	if !crossSubOtherBucket {
		t.Error("two subscriptions of one customer billing the same usage instant of a meter under different prices are NOT caught: the same-price requirement must apply within one subscription only")
	}
	for name, caught := range want {
		if !caught {
			t.Errorf("seeded violation NOT caught by %s — the check's SQL matches nothing", name)
			for _, e := range res.Errors {
				t.Logf("  check error: %v", e)
			}
			for _, v := range res.Violations {
				t.Logf("  saw: %s %s %s", v.Check.Name, v.RowID, v.Detail)
			}
		}
	}
}

// TestDoctor_CreditCurrencyChecks (ADR-129, T11): the three ledger-currency
// checks fire on hand-seeded drift and stay silent on the matching clean rows.
//   - balance_exceeds_block_capacity is per currency: a USD phantom hidden by
//     EUR headroom is still caught.
//   - ledger_currency_matches_invoice: a row tied to an invoice (usage,
//     credit-note grant, reversal by invoice_id; a commit grant by
//     source_invoice_id) carries that invoice's currency.
//   - drained_block_currency_matches: every block a drain names has the
//     draining entry's currency.
func TestDoctor_CreditCurrencyChecks(t *testing.T) {
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

	tenantID := testutil.CreateTestTenant(t, db, "Doctor Credit Currency")
	mustExec := func(q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("seed begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatalf("set livemode: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}
	for _, c := range []string{"cus_dccy_cap", "cus_dccy_inv", "cus_dccy_drain"} {
		mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ($1, $2, $1, $1, '')`, c, tenantID)
	}
	mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end)
		VALUES ('hc_inv_dccy', $1, 'cus_dccy_inv', 'DCCY-1', 'finalized', 'pending', 'EUR', 5000, 5000, 5000, now() - interval '30 days', now())`, tenantID)
	ledger := func(id, cust, typ, currency string, amount, consumed int64, invoiceID, sourceInvoiceID, kind, meta string) {
		t.Helper()
		mustExec(`INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, currency, amount_cents, consumed_cents,
				balance_after, description, invoice_id, source_invoice_id, grant_kind, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 0, 'seed', NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), $11::jsonb)`,
			id, tenantID, cust, typ, currency, amount, consumed, invoiceID, sourceInvoiceID, kind, meta)
	}
	// Capacity: USD sums to 1000 with no open USD block (phantom); EUR has
	// 2000 open against a 1000 balance. Summed across currencies they hide
	// each other.
	ledger("hc_ccl_dccy_cap_usd", "cus_dccy_cap", "grant", "USD", 1000, 1000, "", "", "", `{}`)
	ledger("hc_ccl_dccy_cap_eur", "cus_dccy_cap", "grant", "EUR", 2000, 0, "", "", "", `{}`)
	ledger("hc_ccl_dccy_cap_use", "cus_dccy_cap", "usage", "EUR", -1000, 0, "hc_inv_dccy", "", "", `{}`)
	// Invoice currency: a USD usage row and a USD commit grant on the EUR
	// invoice; a EUR usage row is the clean control.
	ledger("hc_ccl_dccy_bad_use", "cus_dccy_inv", "usage", "USD", -100, 0, "hc_inv_dccy", "", "", `{}`)
	ledger("hc_ccl_dccy_bad_commit", "cus_dccy_inv", "grant", "USD", 9000, 0, "", "hc_inv_dccy", "commit", `{}`)
	ledger("hc_ccl_dccy_ok_use", "cus_dccy_inv", "usage", "EUR", -100, 0, "hc_inv_dccy", "", "", `{}`)
	// Drained blocks: a USD deduction that names a EUR block; a EUR one that
	// names it is the clean control.
	ledger("hc_ccl_dccy_blk", "cus_dccy_drain", "grant", "EUR", 1000, 600, "", "", "", `{}`)
	ledger("hc_ccl_dccy_bad_drain", "cus_dccy_drain", "adjustment", "USD", -300, 0, "", "", "",
		`{"drained_blocks":[{"block_id":"hc_ccl_dccy_blk","take_cents":300}]}`)
	ledger("hc_ccl_dccy_ok_drain", "cus_dccy_drain", "adjustment", "EUR", -300, 0, "", "", "",
		`{"drained_blocks":[{"block_id":"hc_ccl_dccy_blk","take_cents":300}]}`)

	res := doctor.Run(ctx, admin, doctor.Checks)
	for _, e := range res.Errors {
		t.Errorf("check failed to run: %v", e)
	}
	flagged := map[string]map[string]bool{}
	for _, v := range res.Violations {
		if flagged[v.Check.Name] == nil {
			flagged[v.Check.Name] = map[string]bool{}
		}
		flagged[v.Check.Name][v.RowID+" "+v.Detail] = true
	}
	has := func(check, part string) bool {
		for k := range flagged[check] {
			if strings.Contains(k, part) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		check, part string
		want        bool
	}{
		{"balance_exceeds_block_capacity", "customer_id=cus_dccy_cap currency=USD", true},
		{"balance_exceeds_block_capacity", "customer_id=cus_dccy_cap currency=EUR", false},
		{"ledger_currency_matches_invoice", "hc_ccl_dccy_bad_use", true},
		{"ledger_currency_matches_invoice", "hc_ccl_dccy_bad_commit", true},
		{"ledger_currency_matches_invoice", "hc_ccl_dccy_ok_use", false},
		{"drained_block_currency_matches", "hc_ccl_dccy_bad_drain", true},
		{"drained_block_currency_matches", "hc_ccl_dccy_ok_drain", false},
	} {
		if got := has(c.check, c.part); got != c.want {
			t.Errorf("%s flags %q: %v, want %v (flagged: %v)", c.check, c.part, got, c.want, flagged[c.check])
		}
	}
}

// TestDoctor_ImportedInvoicesAreNotFlagged: the Stripe importer (removed
// 2026-04-29) wrote invoices with a stripe_invoice_id and without the
// components Hardcount computes, so their totals, due amounts and timestamps
// do not follow Hardcount's rules. A database that ran it still holds them, and
// the invoice checks must not report them.
func TestDoctor_ImportedInvoicesAreNotFlagged(t *testing.T) {
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

	tenantID := testutil.CreateTestTenant(t, db, "Doctor Imported")
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `INSERT INTO customers (id, tenant_id, external_id, display_name, email)
		VALUES ('cus_doc_imp', $1, 'doc_imp', 'Imp', '')`, tenantID); err != nil {
		t.Fatal(err)
	}
	// Total 4000 against a subtotal of 3000, paid with 4000 still due and
	// no paid_at: each breaks an invoice check on a Hardcount-native row.
	if _, err := tx.ExecContext(ctx, `INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status,
			currency, subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
			billing_period_start, billing_period_end, stripe_invoice_id)
		VALUES ('hc_inv_doc_imp', $1, 'cus_doc_imp', 'IMP-1', 'paid', 'succeeded', 'USD', 3000, 4000, 4000, 4000, 0,
			now() - interval '30 days', now(), 'in_doc_imp')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	res := doctor.Run(ctx, admin, doctor.Checks)
	for _, e := range res.Errors {
		t.Errorf("check failed to run: %v", e)
	}
	for _, v := range res.Violations {
		if strings.Contains(v.RowID+v.Detail, "doc_imp") {
			t.Errorf("%s flagged an imported invoice: %s (%s)", v.Check.Name, v.RowID, v.Detail)
		}
	}
}
