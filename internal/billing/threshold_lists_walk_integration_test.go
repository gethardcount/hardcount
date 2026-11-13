package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedThresholdSubs inserts n active subscriptions with a spend threshold,
// on clockID when it is not empty. Their ids share one prefix and end in
// _0 to _{n-1}, so id order is index order; they are inserted in the reverse
// of it, and the first half by id are touched last.
func seedThresholdSubs(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, clockID string, n int) []string {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	prefix := postgres.NewID("hc_sub")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var clock any
	if clockID != "" {
		clock = clockID
		if _, err := tx.ExecContext(ctx, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status) VALUES ($1, $2, 'walk', $3, 'advancing')`,
			clockID, tenantID, now); err != nil {
			t.Fatalf("seed clock: %v", err)
		}
	}
	ids := make([]string, n)
	for i := range n {
		ids[i] = fmt.Sprintf("%s_%d", prefix, i)
	}
	for i := n - 1; i >= 0; i-- { // inserted in the reverse of id order
		custID := postgres.NewID("hc_cus")
		updated := now
		if i < n/2 {
			updated = now.Add(time.Hour) // the first by id were touched last
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
			VALUES ($1, $2, $3, 'Walk', '', $4, $5, $5)`, custID, tenantID, "walk-"+ids[i], clock, now); err != nil {
			t.Fatalf("seed customer: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (
				id, tenant_id, code, display_name, customer_id, test_clock_id, status, billing_time,
				current_billing_period_start, current_billing_period_end, next_billing_at,
				billing_threshold_amount_gte, created_at, updated_at
			) VALUES ($1, $2, $3, 'Walk Sub', $4, $5, 'active', 'anniversary', $6, $7, $7, 50000, $6, $8)`,
			ids[i], tenantID, "code-"+ids[i], custID, clock, now.Add(-24*time.Hour), now.Add(29*24*time.Hour), updated); err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return ids
}

// walkThresholds walks a threshold list as the scans do, with their key, and
// counts how many times each subscription comes back.
func walkThresholds(t *testing.T, ctx context.Context, name string, list walk.List[domain.Subscription]) map[string]int {
	t.Helper()
	seen := map[string]int{}
	for sub, err := range walk.Walk(ctx, name, list, billing.ThresholdCandidateKey, 2) {
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		seen[sub.ID]++
	}
	return seen
}

// The threshold scans walk their lists by id. Walking a list returns each
// subscription once when id order disagrees with the order the rows were
// inserted in and the order they were touched in. A page in another order
// would end on a late id and leave earlier ones behind the cursor.
func TestListWithThresholds_WalkReturnsEveryRowOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Threshold List Walk")
	store := subscription.NewPostgresStore(db)
	ids := seedThresholdSubs(t, db, ctx, tenantID, "", 5)

	// The real-time list reads every tenant in the mode, so other tests' rows
	// are walked too; only ours are counted.
	seen := walkThresholds(t, ctx, "ListWithThresholds", func(ctx context.Context, after walk.Key, limit int) ([]domain.Subscription, error) {
		return store.ListWithThresholds(ctx, false, after.ID, limit)
	})
	for i, id := range ids {
		if seen[id] != 1 {
			t.Errorf("subscription %d of %d by id returned %d times, want once", i+1, len(ids), seen[id])
		}
	}
}

func TestListWithThresholdsForClock_WalkReturnsEveryRowOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Threshold List Walk")
	store := subscription.NewPostgresStore(db)
	clockID := postgres.NewID("hc_tclk")
	ids := seedThresholdSubs(t, db, ctx, tenantID, clockID, 5)

	seen := walkThresholds(t, ctx, "ListWithThresholdsForClock", func(ctx context.Context, after walk.Key, limit int) ([]domain.Subscription, error) {
		return store.ListWithThresholdsForClock(ctx, tenantID, clockID, after.ID, limit)
	})
	if len(seen) != len(ids) {
		t.Errorf("rows walked = %d, want the clock's %d", len(seen), len(ids))
	}
	for i, id := range ids {
		if seen[id] != 1 {
			t.Errorf("subscription %d of %d by id returned %d times, want once", i+1, len(ids), seen[id])
		}
	}
}
