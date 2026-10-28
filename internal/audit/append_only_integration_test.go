package audit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestAuditLog_AppendOnly exercises the HYG-5 trigger: UPDATE and DELETE
// against audit_log must raise a Postgres exception at the BEFORE-FOR-EACH-ROW
// layer, regardless of whether the caller holds RLS bypass. RLS is the first
// line of defense; this trigger is the belt to its braces.
func TestAuditLog_AppendOnly(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Append Only Audit")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 10*time.Second)
	defer cancel()

	logger := audit.NewLogger(db)
	if err := logger.Log(ctx, tenantID, "invoice.finalize", "invoice", "hc_inv_test", "", nil); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}

	// Pull the inserted row's id so we can target it. Use TxBypass so the
	// test driver doesn't depend on the RLS tenant-match path for read —
	// RLS correctness is exercised by other suites.
	var auditID string
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM audit_log WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1`,
		tenantID,
	).Scan(&auditID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("read audit id: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit read tx: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "UPDATE blocked",
			sql:  `UPDATE audit_log SET action = 'tampered' WHERE id = $1`,
			args: []any{auditID},
		},
		{
			name: "DELETE blocked",
			sql:  `DELETE FROM audit_log WHERE id = $1`,
			args: []any{auditID},
		},
		{
			// 0115: row-level triggers never fire on TRUNCATE, so without a
			// statement-level BEFORE TRUNCATE trigger the app role could wipe
			// the entire log in one statement. Runs inside the rolled-back
			// TxBypass tx, so a regression here cannot actually erase data.
			name: "TRUNCATE blocked",
			sql:  `TRUNCATE audit_log`,
			args: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin tx: %v", err)
			}
			defer postgres.Rollback(tx)

			_, err = tx.ExecContext(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Fatalf("expected trigger to block %s, got nil error", tc.name)
			}
			if !strings.Contains(err.Error(), "append-only") {
				t.Errorf("%s: error did not mention append-only: %v", tc.name, err)
			}
		})
	}

	// Original row must still exist and be unchanged.
	tx2, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin verify tx: %v", err)
	}
	defer postgres.Rollback(tx2)
	var action string
	if err := tx2.QueryRowContext(ctx,
		`SELECT action FROM audit_log WHERE id = $1`, auditID,
	).Scan(&action); err != nil {
		t.Fatalf("re-read audit row: %v", err)
	}
	if action != "invoice.finalize" {
		t.Fatalf("action changed despite trigger: got %q, want %q", action, "invoice.finalize")
	}
}
