package subscription_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A clock's trial and pause jobs read every due row in one call: a clock has
// no next tick, so a job that stopped after one page left the clock ready
// with rows undone. Two pages and one row more prove the cursor moves on.
const clockRows = 2*100 + 1

// seedClockSubs inserts n subscriptions on one test clock, each with its own
// customer, and returns the clock and the subscription ids. trialing makes
// each a trial that ended an hour ago; otherwise each is active with a
// collection pause that ended an hour ago.
func seedClockSubs(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, n int, trialing bool) (string, time.Time, []string) {
	t.Helper()
	frozen := time.Now().UTC().Truncate(time.Microsecond)
	ended := frozen.Add(-time.Hour)
	clockID := postgres.NewID("hc_tclk")
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status) VALUES ($1, $2, 'drain', $3, 'advancing')`,
		clockID, tenantID, frozen); err != nil {
		t.Fatalf("seed clock: %v", err)
	}
	ids := make([]string, n)
	for i := range n {
		custID, subID := postgres.NewID("hc_cus"), postgres.NewID("hc_sub")
		ids[i] = subID
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
			VALUES ($1, $2, $3, 'Drain', '', $4, $5, $5)`, custID, tenantID, fmt.Sprintf("drain-%d-%s", i, subID), clockID, frozen); err != nil {
			t.Fatalf("seed customer: %v", err)
		}
		status, periodEnd := "active", frozen.Add(30*24*time.Hour)
		var trialStart, trialEnd, pauseBehavior, pauseResumes any
		if trialing {
			status, trialStart, trialEnd = "trialing", ended.Add(-14*24*time.Hour), ended
			periodEnd = ended
		} else {
			pauseBehavior, pauseResumes = "keep_as_draft", ended
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO subscriptions (
				id, tenant_id, code, display_name, customer_id, test_clock_id, status, billing_time,
				trial_start_at, trial_end_at,
				current_billing_period_start, current_billing_period_end, next_billing_at,
				pause_collection_behavior, pause_collection_resumes_at, created_at, updated_at
			) VALUES ($1, $2, $3, 'Drain Sub', $4, $5, $6, 'anniversary', $7, $8, $9, $10, $10, $11, $12, $9, $9)`,
			subID, tenantID, "code-"+subID, custID, clockID, status, trialStart, trialEnd,
			frozen.Add(-24*time.Hour), periodEnd, pauseBehavior, pauseResumes); err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return clockID, frozen, ids
}

func TestProcessExpiredTrialsForClock_DoesEveryPage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Trials Every Page")
	store := subscription.NewPostgresStore(db)
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, clockRows, true)

	svc := subscription.NewService(store, clock.Real())
	n, errs := svc.ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if n != clockRows {
		t.Errorf("trials ended = %d, want %d: the job stopped before the last page", n, clockRows)
	}
	for _, id := range []string{ids[0], ids[100], ids[clockRows-1]} {
		if sub, err := store.Get(ctx, tenantID, id); err != nil || sub.Status != domain.SubscriptionActive {
			t.Errorf("sub %s: status %q err %v, want active", id, sub.Status, err)
		}
	}
}

func TestProcessExpiredPauseCollectionsForClock_DoesEveryPage(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Pauses Every Page")
	store := subscription.NewPostgresStore(db)
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, clockRows, false)

	svc := subscription.NewService(store, clock.Real())
	n, errs := svc.ProcessExpiredPauseCollectionsForClock(ctx, tenantID, clockID, frozen)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if n != clockRows {
		t.Errorf("pauses resumed = %d, want %d: the job stopped before the last page", n, clockRows)
	}
	for _, id := range []string{ids[0], ids[100], ids[clockRows-1]} {
		if sub, err := store.Get(ctx, tenantID, id); err != nil || sub.PauseCollection != nil {
			t.Errorf("sub %s: pause %+v err %v, want resumed", id, sub.PauseCollection, err)
		}
	}
}

// A row another transaction holds is waited for, not skipped: a clock has
// one runner, and a skipped row would wait for a later pass, if one came.
func TestProcessExpiredTrialsForClock_WaitsForALockedRow(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Trials Locked Row")
	store := subscription.NewPostgresStore(db)
	clockID, frozen, ids := seedClockSubs(t, db, ctx, tenantID, 3, true)

	holder, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer postgres.Rollback(holder)
	if _, err := holder.ExecContext(ctx, `SELECT id FROM subscriptions WHERE id = $1 FOR UPDATE`, ids[1]); err != nil {
		t.Fatalf("lock row: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = holder.Commit()
	}()

	svc := subscription.NewService(store, clock.Real())
	n, errs := svc.ProcessExpiredTrialsForClock(ctx, tenantID, clockID, frozen)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if n != 3 {
		t.Errorf("trials ended = %d, want 3: the locked row was skipped", n)
	}
	if sub, err := store.Get(ctx, tenantID, ids[1]); err != nil || sub.Status != domain.SubscriptionActive {
		t.Errorf("locked sub: status %q err %v, want active", sub.Status, err)
	}
}

// Each clock list returns a row another transaction holds, so its job waits
// for that row at its write instead of skipping it.
func TestClockLists_IncludeARowAnotherTransactionHolds(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Clock Lists Held Row")
	store := subscription.NewPostgresStore(db)
	trialClock, trialFrozen, trialIDs := seedClockSubs(t, db, ctx, tenantID, 3, true)
	pauseClock, pauseFrozen, pauseIDs := seedClockSubs(t, db, ctx, tenantID, 3, false)

	holder, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer postgres.Rollback(holder)
	if _, err := holder.ExecContext(ctx, `SELECT id FROM subscriptions WHERE id = ANY($1) FOR UPDATE`, []string{trialIDs[1], pauseIDs[1]}); err != nil {
		t.Fatalf("lock rows: %v", err)
	}

	lists := map[string]func() ([]domain.Subscription, error){
		"trials": func() ([]domain.Subscription, error) {
			return store.ListExpiredTrialsForClock(ctx, tenantID, trialClock, trialFrozen, "", 100)
		},
		"pauses": func() ([]domain.Subscription, error) {
			return store.ListExpiredPauseCollectionsForClock(ctx, tenantID, pauseClock, pauseFrozen, "", 100)
		},
		"period closes": func() ([]domain.Subscription, error) {
			return store.GetDueBillingForClock(ctx, tenantID, trialClock, nil, 100)
		},
	}
	for name, list := range lists {
		subs, err := list()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(subs) != 3 {
			t.Errorf("%s list returned %d rows, want 3: the held row was skipped", name, len(subs))
		}
	}
}
