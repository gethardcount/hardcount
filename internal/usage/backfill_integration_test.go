package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
)

// TestBackfill_PersistsOriginAndAggregates is the migration + SQL contract
// test for FEAT-7: the origin column is written correctly, reads back, and
// backfilled events are picked up by the same AggregateForBillingPeriod
// query that billing uses to generate invoices.
func TestBackfill_PersistsOriginAndAggregates(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Backfill Test")
	customerID := insertTestCustomer(t, db, tenantID, "cus_backfill_1")
	meterID := insertTestMeter(t, db, tenantID, "mtr_backfill_1", "api_calls")

	store := usage.NewPostgresStore(db)
	svc := usage.NewService(store)

	// Event 1: live API ingest — should default to origin='api'.
	apiEvt, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(10),
	})
	if err != nil {
		t.Fatalf("api ingest: %v", err)
	}
	if apiEvt.Origin != domain.UsageOriginAPI {
		t.Errorf("api event origin: got %q, want %q", apiEvt.Origin, domain.UsageOriginAPI)
	}

	// Event 2: backfill from 5 days ago.
	past := time.Now().Add(-5 * 24 * time.Hour).UTC()
	backfillEvt, err := svc.Backfill(ctx, tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(100), Timestamp: &past,
	})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if backfillEvt.Origin != domain.UsageOriginBackfill {
		t.Errorf("backfill event origin: got %q, want %q", backfillEvt.Origin, domain.UsageOriginBackfill)
	}

	// Aggregation — both events fall in a period that spans the last 7 days.
	from := time.Now().Add(-7 * 24 * time.Hour)
	to := time.Now().Add(1 * time.Hour)
	totals, err := store.AggregateForBillingPeriod(ctx, tenantID, customerID, []string{meterID}, from, to)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got, want := totals[meterID], decimal.NewFromInt(110); !got.Equal(want) {
		t.Errorf("aggregate includes backfill+api: got %s, want %s (10 api + 100 backfill)", got.String(), want.String())
	}

	// Round-trip: read via List and verify Origin survives the scan.
	verifyOriginSurvivesSelect(t, db, tenantID, apiEvt.ID, domain.UsageOriginAPI)
	verifyOriginSurvivesSelect(t, db, tenantID, backfillEvt.ID, domain.UsageOriginBackfill)
}

// verifyOriginSurvivesSelect reads the origin column directly, because the
// PostgresStore.List scanner doesn't currently project origin — adding that
// is out of scope for FEAT-7 but the column must be readable.
func verifyOriginSurvivesSelect(t *testing.T, db *postgres.DB, tenantID, eventID string, want domain.UsageEventOrigin) {
	t.Helper()
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)

	var got string
	err = tx.QueryRowContext(context.Background(),
		`SELECT origin FROM usage_events WHERE id = $1`, eventID).Scan(&got)
	if err != nil {
		t.Fatalf("select origin for %s: %v", eventID, err)
	}
	if domain.UsageEventOrigin(got) != want {
		t.Errorf("origin on %s: got %q, want %q", eventID, got, want)
	}
}

// ---- minimal tenant-scoped fixtures ---------------------------------------
//
// testutil only exposes CreateTestTenant; customers / meters are inserted
// inline here because FEAT-7 is the first usage integration test and we don't
// want to generalise helpers the rest of the suite doesn't need yet.

func insertTestCustomer(t *testing.T, db *postgres.DB, tenantID, externalID string) string {
	t.Helper()

	id := postgres.NewID("hc_cus")
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin cust: %v", err)
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(context.Background(), `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email)
		VALUES ($1, $2, $3, $4, $5)
	`, id, tenantID, externalID, "Test Customer", externalID+"@example.com")
	if err != nil {
		t.Fatalf("insert cust: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cust: %v", err)
	}
	return id
}

func insertTestMeter(t *testing.T, db *postgres.DB, tenantID, name, key string) string {
	t.Helper()

	id := postgres.NewID("hc_mtr")
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin meter: %v", err)
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(context.Background(), `
		INSERT INTO meters (id, tenant_id, name, event_name, unit, aggregation)
		VALUES ($1, $2, $3, $4, 'requests', 'sum')
	`, id, tenantID, name, key)
	if err != nil {
		t.Fatalf("insert meter: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit meter: %v", err)
	}
	return id
}
