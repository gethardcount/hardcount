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

// paid_commit_invoice_without_grant lists exactly the paid invoices with a
// commit line and no commit grant: the customer paid for credit and got
// none. The rows are written directly, because no writer makes the flagged
// one: every payment of a top-up grants in its own transaction.
func TestDoctor_PaidCommitInvoiceWithoutGrant(t *testing.T) {
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
		if c.Name == "paid_commit_invoice_without_grant" {
			check = append(check, c)
		}
	}
	if len(check) != 1 {
		t.Fatal("check paid_commit_invoice_without_grant not found")
	}

	tenantID := testutil.CreateTestTenant(t, db, "Doctor Commit Grant")
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
	seed(`INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ('cus_cgdoc', $1, 'cgdoc', 'CG Doc', '')`, tenantID)
	invoice := func(id, status, number string, commit bool, granted bool) {
		seed(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status, currency, billing_reason,
				subtotal_cents, total_amount_cents, amount_due_cents, billing_period_start, billing_period_end, paid_at)
			VALUES ($1, $2, 'cus_cgdoc', NULLIF($3, ''), $4, 'pending', 'USD', 'topup', 9000, 9000, 9000, now(), now(),
				CASE WHEN $4 = 'paid' THEN now() END)`, id, tenantID, number, status)
		var grant any
		if commit {
			grant = int64(10000)
		}
		seed(`INSERT INTO invoice_line_items (invoice_id, tenant_id, line_type, description, quantity, unit_amount_cents,
				amount_cents, total_amount_cents, commit_granted_cents)
			VALUES ($1, $2, 'add_on', 'Prepaid credit', 1, 9000, 9000, 9000, $3)`, id, tenantID, grant)
		if granted {
			seed(`INSERT INTO customer_credit_ledger (tenant_id, customer_id, entry_type, amount_cents, balance_after,
					description, currency, invoice_id, source_invoice_id, grant_kind)
				VALUES ($2, 'cus_cgdoc', 'grant', 10000, 10000, 'Prepaid commit', 'USD', $1, $1, 'commit')`, id, tenantID)
		}
	}
	invoice("hc_inv_cgdoc_nogrant", "paid", "CG-1", true, false)
	invoice("hc_inv_cgdoc_granted", "paid", "CG-2", true, true)
	invoice("hc_inv_cgdoc_unpaid", "draft", "", true, false)
	invoice("hc_inv_cgdoc_plain", "paid", "CG-3", false, false)

	res := doctor.Run(ctx, admin, check)
	for _, e := range res.Errors {
		t.Fatalf("check failed to run: %v", e)
	}
	var flagged []string
	for _, v := range res.Violations {
		flagged = append(flagged, v.RowID)
	}
	if len(flagged) != 1 || flagged[0] != "hc_inv_cgdoc_nogrant" {
		t.Errorf("flagged %v, want only hc_inv_cgdoc_nogrant: a granted commit, an unpaid top-up and a paid invoice that sold no credit are fine", flagged)
	}
}
