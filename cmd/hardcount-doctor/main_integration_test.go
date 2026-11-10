package main

import (
	"context"
	"database/sql"
	"errors"
	neturl "net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// These tests run the doctor as the database owner on a managed Postgres: a
// login role that holds the tables' owner's privileges but is neither a
// superuser nor BYPASSRLS. Every tenant table has FORCE ROW LEVEL SECURITY,
// so a plain read as that role sees no tenant rows.

const ownerPassword = "doctor-owner-test-pw"

// runDoctor re-runs this test binary as hardcount-doctor with DATABASE_URL
// set to dsn, and returns its output and exit code.
func runDoctor(t *testing.T, test, dsn string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), "HARDCOUNT_DOCTOR_HELPER=main", "DATABASE_URL="+dsn)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("run doctor: %v", err)
	}
	return string(out), 0
}

// ownerRoleURL creates a role that is a member of the role owning the
// tables, and returns a database URL that logs in as it. It checks that the
// role is the case under test: no superuser, no BYPASSRLS, owner rights.
func ownerRoleURL(t *testing.T) string {
	t.Helper()
	admin := testutil.AdminPool(t)
	var owner string
	if err := admin.QueryRowContext(t.Context(),
		`SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = 'invoices'`).Scan(&owner); err != nil {
		t.Fatalf("read table owner: %v", err)
	}
	role := testutil.CreateRole(t, "hardcount_doctor_owner", ownerPassword, "")
	if _, err := admin.ExecContext(t.Context(), "GRANT "+pgx.Identifier{owner}.Sanitize()+" TO "+role); err != nil {
		t.Fatalf("grant %s to %s: %v", owner, role, err)
	}
	var super, bypass, ownerRights bool
	if err := admin.QueryRowContext(t.Context(),
		`SELECT rolsuper, rolbypassrls, pg_has_role($1, $2, 'USAGE') FROM pg_roles WHERE rolname = $1`,
		role, owner).Scan(&super, &bypass, &ownerRights); err != nil {
		t.Fatalf("read role %s: %v", role, err)
	}
	if super || bypass || !ownerRights {
		t.Fatalf("role %s: superuser=%v bypassrls=%v owner rights=%v, want false false true", role, super, bypass, ownerRights)
	}
	adminURL := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if strings.TrimSpace(adminURL) == "" {
		adminURL = "postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable"
	}
	u, err := neturl.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse admin url: %v", err)
	}
	u.User = neturl.UserPassword(role, ownerPassword)
	return u.String()
}

// seed runs q in its own transaction with RLS off, as an operator's psql
// session would: the planted rows are states no writer produces.
func seed(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(t.Context(), `SELECT set_config('app.livemode', 'on', true)`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	if _, err := tx.ExecContext(t.Context(), q, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

// The doctor finds a planted violation when it runs as a non-superuser
// owner, and exits 1 for it.
func TestDoctor_NonSuperuserOwnerFindsAPlantedViolation(t *testing.T) {
	if os.Getenv("HARDCOUNT_DOCTOR_HELPER") == "main" {
		main()
		return
	}
	db := testutil.SetupTestDB(t)
	dsn := ownerRoleURL(t)

	tenantID := testutil.CreateTestTenant(t, db, "Doctor Owner")
	seed(t, db, `INSERT INTO customers (id, tenant_id, external_id, display_name, email)
		VALUES ('cus_doc_owner', $1, 'doc_owner', 'Doc', '')`, tenantID)
	// Paid and succeeded, but with no paid_at and money still due: the
	// settle writer never leaves an invoice like this.
	seed(t, db, `INSERT INTO invoices (id, tenant_id, customer_id, invoice_number, status, payment_status,
			currency, subtotal_cents, total_amount_cents, amount_due_cents, amount_paid_cents, credits_applied_cents,
			billing_period_start, billing_period_end)
		VALUES ('hc_inv_doc_owner', $1, 'cus_doc_owner', 'DOC-OWNER-1', 'paid', 'succeeded', 'USD', 4000, 4000, 4000, 4000, 0,
			now() - interval '30 days', now())`, tenantID)

	// The role is blind without the doctor's own setting: a plain read sees
	// no invoice. Without this, the test could pass on a role that sees
	// every row anyway.
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open owner pool: %v", err)
	}
	defer func() { _ = pool.Close() }()
	var visible int
	if err := pool.QueryRowContext(t.Context(), `SELECT count(*) FROM invoices`).Scan(&visible); err != nil {
		t.Fatalf("plain read as owner: %v", err)
	}
	if visible != 0 {
		t.Fatalf("a plain read as the owner role sees %d invoices, want 0: FORCE ROW LEVEL SECURITY is off, or the role bypasses it", visible)
	}

	out, code := runDoctor(t, "TestDoctor_NonSuperuserOwnerFindsAPlantedViolation", dsn)
	if code != 1 {
		t.Fatalf("exit code %d, want 1 (a violation); output:\n%s", code, out)
	}
	if !strings.Contains(out, "VIOLATION [invoices/invoice_paid_settled_coherence] hc_inv_doc_owner") {
		t.Fatalf("the planted invoice is not reported by invoice_paid_settled_coherence; output:\n%s", out)
	}
	if strings.Contains(out, "CHECK-ERROR") {
		t.Errorf("a check failed to run; output:\n%s", out)
	}
}

// The doctor exits 2, not OK, when tenants exist but it reads no customers
// and no invoices. Once a customer is readable it sweeps as usual.
func TestDoctor_EmptyReadWithTenantsExits2(t *testing.T) {
	if os.Getenv("HARDCOUNT_DOCTOR_HELPER") == "main" {
		main()
		return
	}
	db := testutil.SetupTestDB(t)
	dsn := ownerRoleURL(t)
	tenantID := testutil.CreateTestTenant(t, db, "Doctor Empty")

	out, code := runDoctor(t, "TestDoctor_EmptyReadWithTenantsExits2", dsn)
	if code != 2 {
		t.Fatalf("exit code %d, want 2 (nothing read); output:\n%s", code, out)
	}
	if !strings.Contains(out, "1 tenant(s) exist, but the doctor read no customers and no invoices") {
		t.Errorf("no message names the empty read; output:\n%s", out)
	}
	if strings.Contains(out, "OK — no invariant violations") {
		t.Errorf("the doctor printed OK over an empty read; output:\n%s", out)
	}

	// A customer with no invoice yet is a new account, not an empty read.
	seed(t, db, `INSERT INTO customers (id, tenant_id, external_id, display_name, email)
		VALUES ('cus_doc_empty', $1, 'doc_empty', 'Doc', '')`, tenantID)
	out, code = runDoctor(t, "TestDoctor_EmptyReadWithTenantsExits2", dsn)
	if code != 0 || !strings.Contains(out, "OK — no invariant violations") {
		t.Fatalf("with one customer: exit code %d, want 0 and OK; output:\n%s", code, out)
	}
}
