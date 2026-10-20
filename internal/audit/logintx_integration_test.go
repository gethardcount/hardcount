package audit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
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
		domain.AuditActionActivate, domain.AuditActionCancel, domain.AuditActionPause,
		domain.AuditActionResume, domain.AuditActionFinalize, domain.AuditActionVoid,
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
				Action: action, ResourceType: "vocab_probe", ResourceID: "vlx_probe_1",
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
			Action: "update", ResourceType: "guc_probe", ResourceID: "vlx_probe_2",
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
			Action: "update", ResourceType: "marshal_probe", ResourceID: "vlx_probe_3",
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

	t.Run("sim context lands in columns and legacy metadata keys", func(t *testing.T) {
		simAt := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		// The sim axis is derived from the ctx's clock binding, never passed per
		// emission (ADR-090 §5) — an emitter cannot forget it, and cannot set a
		// clock id that disagrees with the instant.
		simCtx := clock.WithSim(ctx, clock.Sim{At: simAt, TestClockID: "vlx_clk_1"})
		if err := logger.LogInTx(simCtx, tx, audit.Entry{
			Action: "cancel", ResourceType: "sim_probe", ResourceID: "vlx_probe_4",
		}); err != nil {
			t.Fatalf("log: %v", err)
		}
		var colAt time.Time
		var colClock, metaJSON string
		if err := tx.QueryRowContext(ctx,
			`SELECT sim_effective_at, test_clock_id, metadata::text FROM audit_log WHERE resource_type = 'sim_probe'`,
		).Scan(&colAt, &colClock, &metaJSON); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if !colAt.Equal(simAt) || colClock != "vlx_clk_1" {
			t.Errorf("sim columns: got (%v, %q), want (%v, vlx_clk_1) — migration 0148", colAt, colClock, simAt)
		}
		if !strings.Contains(metaJSON, "sim_effective_at") || !strings.Contains(metaJSON, "vlx_clk_1") {
			t.Errorf("legacy metadata mirror missing (dashboard renders these keys today): %s", metaJSON)
		}
		_ = tx.Commit()
	})

	t.Run("wall-clock ctx leaves the sim columns NULL", func(t *testing.T) {
		// The partial index (0148) and every sim filter key on IS NOT NULL, so a
		// wall-clock row must carry SQL NULL — not a zero time, not an empty
		// string, either of which would drag real-world rows into the simulated
		// slice.
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if err := logger.LogInTx(ctx, tx, audit.Entry{
			Action: "update", ResourceType: "wall_probe", ResourceID: "vlx_probe_8",
		}); err != nil {
			t.Fatalf("log: %v", err)
		}
		var colAt *time.Time
		var colClock *string
		if err := tx.QueryRowContext(ctx,
			`SELECT sim_effective_at, test_clock_id FROM audit_log WHERE resource_type = 'wall_probe'`,
		).Scan(&colAt, &colClock); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if colAt != nil || colClock != nil {
			t.Errorf("wall-clock row must have NULL sim columns; got (%v, %v)", colAt, colClock)
		}
		_ = tx.Commit()
	})

	t.Run("a half-set binding is treated as absent", func(t *testing.T) {
		// A clock id with no instant is not a simulation — stamping it would put
		// a row in the partial index whose sim time is unknowable.
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		halfCtx := clock.WithSim(ctx, clock.Sim{TestClockID: "vlx_clk_partial"})
		if err := logger.LogInTx(halfCtx, tx, audit.Entry{
			Action: "update", ResourceType: "half_probe", ResourceID: "vlx_probe_9",
		}); err != nil {
			t.Fatalf("log: %v", err)
		}
		var colClock *string
		if err := tx.QueryRowContext(ctx,
			`SELECT test_clock_id FROM audit_log WHERE resource_type = 'half_probe'`,
		).Scan(&colClock); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if colClock != nil {
			t.Errorf("half-set binding must not stamp the clock; got %v", *colClock)
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
		err = logger.LogInTx(ctx, tx, audit.Entry{
			Action: "update", ResourceType: "gucless_probe", ResourceID: "vlx_probe_6",
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
			Action: "update", ResourceType: "rollback_probe", ResourceID: "vlx_probe_5",
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
