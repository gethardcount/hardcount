package doctor_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/doctor"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// invoice_tax_calculated_never_committed flags an invoice Stripe Tax
// calculated whose sale was never recorded at Stripe, with or without tax on
// it. A transaction with no tax still records the sale in the tenant's Stripe
// Tax reports, and the tax-commit sweep commits it like any other. The check
// skipped zero-tax invoices, on the belief that they had nothing to report.
//
// It states the sweep's set (ListPendingTaxCommit): a draft voided before it
// was issued was never a sale, the sweep never commits it, and the check
// does not flag it.
func TestDoctor_TaxCalculatedNeverCommitted_ZeroTaxToo(t *testing.T) {
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

	var check []doctor.Check
	for _, c := range doctor.Checks {
		if c.Name == "invoice_tax_calculated_never_committed" {
			check = append(check, c)
		}
	}
	if len(check) != 1 {
		t.Fatal("check invoice_tax_calculated_never_committed not found")
	}

	tenantID := testutil.CreateTestTenant(t, db, "Doctor Tax Never Committed")
	seed := func(q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("seed begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatalf("set livemode: %v", err)
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}
	seed(`INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ('cus_taxdoc', $1, 'taxdoc', 'Tax Doc', '')`, tenantID)
	invoice := func(id, status string, issued bool, taxCents int64, taxTx string) {
		seed(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency,
				subtotal_cents, tax_amount_cents, total_amount_cents, amount_due_cents,
				tax_provider, tax_status, tax_calculation_id, tax_transaction_id,
				billing_period_start, billing_period_end, issued_at, updated_at)
			VALUES ($1, $2, 'cus_taxdoc', $1, $5, 'pending', 'USD', 10000, $3::bigint, 10000 + $3::bigint, 10000 + $3::bigint,
				'stripe_tax', 'ok', 'taxcalc_' || $1::text, NULLIF($4::text, ''),
				now() - interval '33 days', now() - interval '3 days',
				CASE WHEN $6 THEN now() - interval '3 days' END, now() - interval '3 days')`,
			id, tenantID, taxCents, taxTx, status, issued)
	}
	invoice("hc_inv_taxdoc_zero", "finalized", true, 0, "")
	invoice("hc_inv_taxdoc_taxed", "finalized", true, 1000, "")
	invoice("hc_inv_taxdoc_committed", "finalized", true, 0, "tax_recorded")
	invoice("hc_inv_taxdoc_issued_voided", "voided", true, 0, "")
	invoice("hc_inv_taxdoc_draft_voided", "voided", false, 1000, "")

	res := doctor.Run(ctx, admin, check)
	for _, e := range res.Errors {
		t.Fatalf("check failed to run: %v", e)
	}
	flagged := map[string]bool{}
	for _, v := range res.Violations {
		flagged[v.RowID] = true
	}
	if !flagged["hc_inv_taxdoc_zero"] {
		t.Error("a zero-tax invoice Stripe Tax calculated and never committed is not flagged: its sale is not on the Stripe Tax report")
	}
	if !flagged["hc_inv_taxdoc_taxed"] {
		t.Error("a taxed invoice never committed is not flagged")
	}
	if flagged["hc_inv_taxdoc_committed"] {
		t.Error("a committed invoice is flagged")
	}
	if !flagged["hc_inv_taxdoc_issued_voided"] {
		t.Error("an invoice issued, then voided, never committed is not flagged: its sale is not on the report")
	}
	if flagged["hc_inv_taxdoc_draft_voided"] {
		t.Error("a draft voided before it was issued is flagged: it was never a sale, and the sweep rightly never commits it")
	}
}
