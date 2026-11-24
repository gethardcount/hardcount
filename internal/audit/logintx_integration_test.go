package audit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// LogInTx totality gates (ADR-090, panel amendment A2). LogInTx runs inside
// money transactions — an input it cannot persist aborts the mutation, so
// every failure mode a caller could trigger must be either unrepresentable
// or absorbed. These tests run against real Postgres because the guarantees
// live in the schema (NOT NULLs, CHECK constraints, trigger, GUCs).
func TestLogInTx_TotalityAndVocabulary(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "LogInTx Totality")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	logger := audit.NewLogger(db)

	// Every declared action wire-string must survive the full INSERT path —
	// schema CHECK drift on a new constant fails HERE, not in a customer's
	// money tx (the enum+CHECK-constraint-sync bug class).
	vocabulary := []string{
		domain.AuditActionCreate, domain.AuditActionUpdate, domain.AuditActionDelete,
		domain.AuditActionActivate, domain.AuditActionCancel,
		domain.AuditActionFinalize, domain.AuditActionVoid,
		domain.AuditActionRevoke, domain.AuditActionGrant, domain.AuditActionRefund,
		domain.AuditActionCollect, domain.AuditActionSend, domain.AuditActionRetryTax,
		domain.AuditActionRotate, domain.AuditActionRun,
		// The read-egress action (ADR-090 §7): action=export on the bulk CSV
		// streams. A new TOP-LEVEL wire string, so it must survive the same
		// INSERT path every mutating action does — this is the gate that would
		// have caught it if audit_log had ever grown an action CHECK.
		domain.AuditActionExport,
		// Dotted + auth service vocabulary in live use (frozen wire strings —
		// swept from every .Log call site; the totality gate must cover the
		// FULL vocabulary or a schema rejection ships to a money tx).
		"credit.adjustment", "credit.deduction",
		"credit_note.issued",
		// ADR-081 membership: all four, not just member.joined — the list
		// claimed to be a full sweep while three of them were missing.
		"member.invited", "member.joined", "member.invite_revoked", "member.removed",
		"subscription.item_updated", "subscription.pending_change_applied",
		"subscription.proration_failed", "subscription.threshold_crossed",
		"subscription.threshold_deferred", "subscription.net_negative_usage",
		"login", "logout", "mode_changed",
		"password_reset_requested", "password_reset_completed",
	}
	t.Run("vocabulary round-trip", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		for _, action := range vocabulary {
			if err := logger.LogInTx(ctx, tx, audit.Entry{
				Action: action, ResourceType: "vocab_probe", ResourceID: "hc_probe_1",
			}); err != nil {
				t.Fatalf("vocabulary value %q rejected by the schema: %v", action, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	})

	t.Run("tenant_id comes from the tx GUC, not the caller", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := logger.LogInTx(ctx, tx, audit.Entry{
			Action: "update", ResourceType: "guc_probe", ResourceID: "hc_probe_2",
		}); err != nil {
			t.Fatalf("log: %v", err)
		}
		var gotTenant string
		var gotLivemode bool
		if err := tx.QueryRowContext(ctx,
			`SELECT tenant_id, livemode FROM audit_log WHERE resource_type = 'guc_probe'`,
		).Scan(&gotTenant, &gotLivemode); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if gotTenant != tenantID {
			t.Errorf("tenant_id: got %q, want the tx GUC's %q", gotTenant, tenantID)
		}
		if gotLivemode != false {
			t.Errorf("livemode: got %v, want false (trigger-stamped from the tx session)", gotLivemode)
		}
		_ = tx.Commit()
	})

	t.Run("unmarshalable metadata degrades, never aborts", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := logger.LogInTx(ctx, tx, audit.Entry{
			Action: "update", ResourceType: "marshal_probe", ResourceID: "hc_probe_3",
			Metadata: map[string]any{"bad": func() {}}, // json.Marshal error
		}); err != nil {
			t.Fatalf("unmarshalable metadata must degrade, not abort the tx: %v", err)
		}
		var metaJSON string
		if err := tx.QueryRowContext(ctx,
			`SELECT metadata::text FROM audit_log WHERE resource_type = 'marshal_probe'`,
		).Scan(&metaJSON); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !strings.Contains(metaJSON, "marshal_error") {
			t.Errorf("degraded metadata must carry marshal_error; got %s", metaJSON)
		}
		_ = tx.Commit()
	})

	t.Run("GUC-less tx fails loudly on NOT NULL, not the incidental FK", func(t *testing.T) {
		// TxBypass sets no app.tenant_id. On a pooled connection the
		// reverted GUC placeholder is an EMPTY STRING (not NULL), which
		// would pass NOT NULL and land an RLS-invisible orphan row if the
		// INSERT didn't fold '' to NULL. Pin the loud failure and its class.
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		// Set the mode so the insert reaches the tenant guard: the livemode
		// trigger refuses an unset mode before NOT NULL is checked.
		if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'on', true)`); err != nil {
			t.Fatalf("set livemode: %v", err)
		}
		err = logger.LogInTx(ctx, tx, audit.Entry{
			Action: "update", ResourceType: "gucless_probe", ResourceID: "hc_probe_6",
		})
		if err == nil {
			t.Fatal("LogInTx on a GUC-less tx must fail loudly")
		}
		if !strings.Contains(err.Error(), "null value") {
			t.Errorf("failure class must be the NOT NULL constraint (documented guard), got: %v", err)
		}
	})

	t.Run("rollback takes the audit row with it — shared fate", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := logger.LogInTx(ctx, tx, audit.Entry{
			Action: "update", ResourceType: "rollback_probe", ResourceID: "hc_probe_5",
		}); err != nil {
			t.Fatalf("log: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		entries, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceType: "rollback_probe"})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("phantom audit row survived a rolled-back tx: %+v", entries)
		}
	})
}
