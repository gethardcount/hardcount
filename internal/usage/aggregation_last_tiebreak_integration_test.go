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

// TestAggregateLast_TimestampTieBreaksByID: a `last` meter bills its latest
// event by (timestamp, id), on every read path (ADR-130 §2). Two events at
// the same instant are a tie on the timestamp, so the one with the higher id
// is the latest. The close of a meter with no pricing rules reads its total
// through AggregateForBillingPeriodByAgg, which broke the tie by id; the
// previews, the live balance and every meter with pricing rules read through
// the claim path, which did not. So a preview could show one value and the
// invoice bill another.
//
// The events are written lower id first, so an order that ignores the id
// keeps the scan's order and picks the lower id.
func TestAggregateLast_TimestampTieBreaksByID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()
	store := usage.NewPostgresStore(db)

	at := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	from, to := at.Add(-time.Hour), at.Add(time.Hour)

	setup := func(t *testing.T, name string) (tenantID, customerID, meterID string) {
		t.Helper()
		tenantID = testutil.CreateTestTenant(t, db, name)
		customerID = insertTestCustomer(t, db, tenantID, "cus_tie")
		meterID = insertTestMeter(t, db, tenantID, "Gauge", "gauge_tie")
		tenantExec(t, db, tenantID, `UPDATE meters SET aggregation = 'last' WHERE id = $1`, meterID)
		// Lower id first: 7 at hc_evt_tie_a, then 9 at hc_evt_tie_b, same instant.
		for _, ev := range []struct{ id, qty string }{{"hc_evt_tie_a", "7"}, {"hc_evt_tie_b", "9"}} {
			tenantExec(t, db, tenantID, `
				INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, timestamp)
				VALUES ($1, $2, $3, $4, $5, '{"model": "a"}', $6)`,
				ev.id+"_"+tenantID, tenantID, customerID, meterID, ev.qty, at)
		}
		return tenantID, customerID, meterID
	}
	want := decimal.NewFromInt(9)

	t.Run("a meter with no pricing rules: the claim path agrees with the close's read", func(t *testing.T) {
		tenantID, customerID, meterID := setup(t, "Last tie, no rules")
		totals, err := store.AggregateForBillingPeriodByAgg(ctx, tenantID, customerID, map[string]string{meterID: "last"}, from, to)
		if err != nil {
			t.Fatalf("meter total: %v", err)
		}
		if !totals[meterID].Equal(want) {
			t.Errorf("the close's read = %s, want %s (the higher id)", totals[meterID], want)
		}
		aggs, err := store.AggregateClaims(ctx, tenantID, customerID, meterID, domain.AggregationLast, nil, from, to)
		if err != nil {
			t.Fatalf("claim path: %v", err)
		}
		assertRuleAggregations(t, aggs, map[string]decimal.Decimal{"": want})
	})

	t.Run("a meter whose pricing rule claims the events", func(t *testing.T) {
		tenantID, customerID, meterID := setup(t, "Last tie, rule")
		rrvID := insertTestRatingRule(t, db, tenantID, "gauge_rate")
		c := insertTestPricingRule(t, db, tenantID, meterID, rrvID, map[string]any{"model": "a"}, 100)
		aggs, err := store.AggregateClaims(ctx, tenantID, customerID, meterID, domain.AggregationLast, []domain.UsageClaim{c}, from, to)
		if err != nil {
			t.Fatalf("claim path: %v", err)
		}
		assertRuleAggregations(t, aggs, map[string]decimal.Decimal{rrvID: want})
	})
}

// tenantExec runs one statement on a transaction scoped to tenantID.
func tenantExec(t *testing.T, db *postgres.DB, tenantID, query string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
