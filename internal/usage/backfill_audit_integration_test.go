package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// Backfill is the ONE ingest path that audits. Live metering (POST
// /v1/usage-events, /batch, the LiteLLM callback) is deliberately exempt —
// usage_events IS the record and a row per event would double the write volume
// of the hottest path. But an operator inserting BACKDATED usage is changing
// what a customer gets billed for a period that may already have closed: a
// money-path action, not machine telemetry. It rides the ingest transaction,
// so a backfilled event that cannot be recorded is not ingested at all.
func TestBackfillAudit_SharedFate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Backfill Audit")

	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	store := usage.NewPostgresStore(db)
	logger := audit.NewLogger(db)

	customerID := insertTestCustomer(t, db, tenantID, "cus_backfill_audit")
	meterID := insertTestMeter(t, db, tenantID, "mtr_backfill_audit", "audit_calls")

	// A timestamp in a period that has already closed — the whole reason this
	// action is audit-worthy.
	backdatedAt := time.Now().UTC().Add(-45 * 24 * time.Hour)

	input := usage.IngestInput{
		CustomerID: customerID,
		MeterID:    meterID,
		Quantity:   decimal.NewFromInt(42),
		Timestamp:  &backdatedAt,
	}

	usageRows := func(t *testing.T) []domain.AuditEntry {
		t.Helper()
		rows, _, err := logger.Query(ctx, tenantID, audit.QueryFilter{ResourceType: "usage_event"})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		return rows
	}

	t.Run("backfill commits the event and its audit row together", func(t *testing.T) {
		svc := usage.NewService(store)
		svc.SetAuditLogger(logger)

		evt, err := svc.Backfill(ctx, tenantID, input)
		if err != nil {
			t.Fatalf("backfill: %v", err)
		}

		rows := usageRows(t)
		if len(rows) != 1 {
			t.Fatalf("want exactly one usage_event audit row; got %+v", rows)
		}
		r := rows[0]
		if r.Action != domain.AuditActionCreate || r.ResourceID != evt.ID {
			t.Errorf("row = %s/%s, want create/%s", r.Action, r.ResourceID, evt.ID)
		}
		if r.Metadata["action"] != "usage_backfilled" {
			t.Errorf("metadata action = %v, want usage_backfilled", r.Metadata["action"])
		}
		if r.Metadata["customer_id"] != customerID {
			t.Errorf("metadata customer_id = %v, want %s", r.Metadata["customer_id"], customerID)
		}
		// The backdated instant is the point — it must be on the row, not just
		// the wall-clock created_at.
		if got, _ := r.Metadata["event_at"].(string); got != backdatedAt.Format(time.RFC3339) {
			t.Errorf("metadata event_at = %q, want the BACKDATED timestamp %q", got, backdatedAt.Format(time.RFC3339))
		}
	})

	t.Run("audit failure rolls the backfilled event back", func(t *testing.T) {
		svc := usage.NewService(store)
		svc.SetAuditLogger(failingProviderCostEmitter{})

		before := len(usageRows(t))

		bad := input
		other := backdatedAt.Add(-time.Hour)
		bad.Timestamp = &other
		if _, err := svc.Backfill(ctx, tenantID, bad); err == nil {
			t.Fatal("backfill must fail when its audit emission fails (shared fate)")
		}

		// The event must NOT be in the ledger: an unrecordable backdated insert
		// is no insert at all.
		events, _, err := store.List(ctx, usage.ListFilter{TenantID: tenantID, CustomerID: customerID, Limit: 100})
		if err != nil {
			t.Fatalf("list usage events: %v", err)
		}
		for _, e := range events {
			if e.Timestamp.UTC().Equal(other.UTC()) {
				t.Fatalf("backfilled event committed without its audit row: %+v", e)
			}
		}
		if after := len(usageRows(t)); after != before {
			t.Errorf("audit rows moved %d→%d on a rolled-back backfill", before, after)
		}
	})

	// Live ingest shares the same store method — it must stay silent, or the
	// hottest path in the product doubles its write volume.
	t.Run("live ingest emits nothing", func(t *testing.T) {
		svc := usage.NewService(store)
		svc.SetAuditLogger(logger)

		before := len(usageRows(t))
		if _, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
			CustomerID: customerID,
			MeterID:    meterID,
			Quantity:   decimal.NewFromInt(7),
		}); err != nil {
			t.Fatalf("live ingest: %v", err)
		}
		if after := len(usageRows(t)); after != before {
			t.Errorf("live machine ingest wrote an audit row (%d→%d) — it is exempt by design", before, after)
		}
	})
}
