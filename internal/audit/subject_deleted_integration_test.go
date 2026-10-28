package audit_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestSubjectDeleted_TheRowOutlivesItsSubject pins the read-time resolution behind
// the dashboard's "View" link.
//
// Tearing down a test clock HARD-DELETES its entire simulated graph (ADR-086) — the
// invoices, the subscriptions, the customers. The audit rows deliberately SURVIVE
// that: they are the only remaining record that the simulation ever happened, and
// 0150 revoked DELETE on audit_log, so they could not be removed even on purpose.
//
// The consequence nobody wired up: those rows still name an invoice ID, and the
// dashboard still rendered a "View" link straight to it. The link 404s. Every time.
//
// subject_deleted is resolved on READ (a LEFT JOIN to test_clocks), never stored —
// the same shape as resource_label. Storing it would need an UPDATE to audit_log,
// which 0150 revoked, so read-time resolution is not a preference here: it is the
// only door left open.
func TestSubjectDeleted_TheRowOutlivesItsSubject(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Subject Deleted")
	logger := audit.NewLogger(db)

	clockID := "hc_clk_teardown"
	seedClock(t, db, ctx, tenantID, clockID)

	simAt := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	simCtx := clock.WithSim(ctx, clock.Sim{At: simAt, TestClockID: clockID})
	if err := logger.Log(simCtx, tenantID, "finalize", "invoice", "hc_inv_sim", "INV-SIM", nil); err != nil {
		t.Fatalf("log sim row: %v", err)
	}
	// A live wall-clock row, as the control: it belongs to no clock, so nothing can
	// ever delete its subject out from under it.
	if err := logger.Log(ctx, tenantID, "finalize", "invoice", "hc_inv_real", "INV-REAL", nil); err != nil {
		t.Fatalf("log real row: %v", err)
	}

	t.Run("while the clock lives, the subject is reachable", func(t *testing.T) {
		for _, e := range queryAll(t, logger, ctx, tenantID) {
			if e.SubjectDeleted {
				t.Errorf("row %s reports subject_deleted while its clock still exists — the dashboard would hide a link that works", e.ResourceID)
			}
		}
	})

	// Teardown. The clock row goes; the audit rows do not.
	execTenant(t, db, ctx, tenantID, `DELETE FROM test_clocks WHERE id = $1`, clockID)

	t.Run("after teardown the sim row survives and admits its subject is gone", func(t *testing.T) {
		rows := queryAll(t, logger, ctx, tenantID)
		if len(rows) != 2 {
			t.Fatalf("got %d audit rows after teardown, want 2 — the rows must OUTLIVE the clock; they are the only record the simulation happened", len(rows))
		}
		for _, e := range rows {
			switch e.ResourceID {
			case "hc_inv_sim":
				if !e.SubjectDeleted {
					t.Error("the torn-down clock's row does not report subject_deleted — the dashboard renders a View link to an invoice that no longer exists, and the operator lands on a 404")
				}
				// The row must not be hollowed out by the teardown either.
				if e.TestClockID != clockID {
					t.Errorf("test_clock_id = %q after teardown, want %q. audit_log.test_clock_id is deliberately a bare TEXT column with NO foreign key (0148): an ON DELETE SET NULL would strip the sim axis off every surviving row and make subject_deleted unresolvable", e.TestClockID, clockID)
				}
				if e.SimEffectiveAt == nil {
					t.Error("sim_effective_at lost on teardown")
				}
			case "hc_inv_real":
				if e.SubjectDeleted {
					t.Error("a live wall-clock row reports subject_deleted — its subject was never in any simulation, so its link still works and must still be offered")
				}
			}
		}
	})
}

func queryAll(t *testing.T, logger *audit.Logger, ctx context.Context, tenantID string) []domainEntry {
	t.Helper()
	got, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	out := make([]domainEntry, 0, len(got))
	for _, e := range got {
		out = append(out, domainEntry{
			ResourceID:     e.ResourceID,
			SubjectDeleted: e.SubjectDeleted,
			TestClockID:    e.TestClockID,
			SimEffectiveAt: e.SimEffectiveAt,
		})
	}
	return out
}

type domainEntry struct {
	ResourceID     string
	SubjectDeleted bool
	TestClockID    string
	SimEffectiveAt *time.Time
}

func seedClock(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, clockID string) {
	t.Helper()
	execTenant(t, db, ctx, tenantID,
		`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, livemode) VALUES ($1, $2, 'teardown fixture', $3, false)`,
		clockID, tenantID, time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC))
}

func execTenant(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, q string, args ...any) {
	t.Helper()
	if err := db.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
