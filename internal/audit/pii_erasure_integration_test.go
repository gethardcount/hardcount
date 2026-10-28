package audit_test

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestResourceLabel_IsResolvedNotStored_SoErasureWorks is the load-bearing proof
// for the whole PII change.
//
// audit_log is append-only and migration 0150 revoked DELETE from the runtime
// role: a row cannot be corrected, redacted or erased by the application, ever.
// Seven writers were putting live email addresses into it — the invitee's on
// member.invited, the operator's on login and password_reset_*, the new member's
// on member.joined, the owner's on bootstrap. That made the audit log the one
// table from which a person's address could never be removed.
//
// The rule now: an audit row POINTS AT a person (resource_id = their user id) and
// never STORES their address. This test proves both halves of what that buys:
//
//  1. the log can still answer "WHO?" — the address renders on read; and
//  2. GDPR erasure actually works — delete the person, and the address is gone
//     from EVERY historical row at once, because it was never in them.
//
// If someone "optimises" this by storing the label at write time, (2) breaks
// silently and nothing else in the suite notices. That is what this test is for.
func TestResourceLabel_IsResolvedNotStored_SoErasureWorks(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "PII Erasure")

	const email = "sam@acme.test"
	var userID string
	if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, email).Scan(&userID)
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	logger := audit.NewLogger(db)

	// The row a member removal writes: it names the user id and NO label — exactly
	// the shape RemoveMember produces. RemoveMember also deletes the user_tenants
	// row, so the tenant's member list can no longer name this person: the audit
	// log is the only thing left that can.
	if err := logger.Log(ctx, tenantID, "member.removed", "user", userID, "", nil); err != nil {
		t.Fatalf("log: %v", err)
	}

	// (1) The log can still answer WHO — resolved on read, not stored.
	rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{Action: "member.removed"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	if rows[0].ResourceLabel != email {
		t.Fatalf("resource_label = %q, want %q — the log cannot say WHO was removed, which is the one question this row exists to answer", rows[0].ResourceLabel, email)
	}

	// Nothing was STORED. Read the raw column: if the address is physically in the
	// table, erasure below is a lie no matter what the API returns.
	var stored string
	if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT COALESCE(resource_label,'') FROM audit_log WHERE action = 'member.removed'`).Scan(&stored)
	}); err != nil {
		t.Fatalf("read raw resource_label: %v", err)
	}
	if strings.Contains(stored, "@") {
		t.Fatalf("resource_label is PHYSICALLY STORED as %q — audit_log is append-only, so that address can never be erased", stored)
	}

	// (2) Erasure. Delete the person; the address must vanish from the history.
	if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
		return err
	}); err != nil {
		t.Fatalf("erase user: %v", err)
	}

	after, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{Action: "member.removed"})
	if err != nil {
		t.Fatalf("query after erasure: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("the audit ROW must survive erasure (it is the tamper-evidence record): got %d rows", len(after))
	}
	if strings.Contains(after[0].ResourceLabel, "@") {
		t.Errorf("after deleting the user, the audit log STILL renders %q — erasure did not reach the append-only table, which is the entire failure this change exists to fix", after[0].ResourceLabel)
	}
}

// TestLog_UnmarshalableMetadata_StillWritesTheRow is the regression lock for a row
// that was being silently DELETED by a telemetry bug.
//
// Log() discarded json.Marshal's error and then guarded `metadata == nil` — the
// WRONG case. On a marshal failure the metadata map is perfectly valid and non-nil;
// it is metaJSON that is nil. Nil binds into a JSONB NOT NULL column, the INSERT
// fails, and ~85 call sites discard Log's error with `_ =`. So one NaN in a
// metadata bag silently erased the audit row for a money mutation.
//
// LogInTx already degraded correctly. Log did not, and nothing noticed.
func TestLog_UnmarshalableMetadata_StillWritesTheRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Marshal Degrade")

	logger := audit.NewLogger(db)

	// math.Inf is not representable in JSON — json.Marshal returns an error and nil
	// bytes. A real telemetry bug looks exactly like this.
	bad := map[string]any{"ratio": math.Inf(1)}

	if err := logger.Log(ctx, tenantID, domain.AuditActionUpdate, "invoice", "hc_inv_marshal", "INV-1", bad); err != nil {
		t.Fatalf("Log returned an error for an unmarshalable bag — it must DEGRADE, never lose the row: %v", err)
	}

	rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceID: "hc_inv_marshal"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the audit row for a money mutation was LOST because its metadata would not marshal: got %d rows, want 1", len(rows))
	}
	if _, ok := rows[0].Metadata["marshal_error"]; !ok {
		t.Errorf("the row landed but does not record WHY its metadata is missing: %+v — degrade loudly, so the gap is visible", rows[0].Metadata)
	}
}
