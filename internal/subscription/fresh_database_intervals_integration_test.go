package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestIntervals_FreshDatabaseNeedsNoBackfill is why the boot-time
// billing-interval backfill was deleted (#1364). Hardcount 0.9.0 does not take
// over a 0.8.x database, so every database starts from the goose baseline,
// after the ADR-101 dual-write shipped. On such a database every writer that
// gives an item a billable lifetime writes its interval in the same
// transaction, so there is nothing to reconstruct.
//
// The test drives every item writer on an empty database, then checks the
// table: every item has an interval, every live item of an active
// subscription has exactly one open one, and no other item has an open one.
//
// Mutation check: drop the intervalOpenForAdd call in addItemInTx, or the
// intervalOpenForItems call in createInTx, and this fails.
func TestIntervals_FreshDatabaseNeedsNoBackfill(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "IV Fresh")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_iv_fresh")
	planA := ivPlan(t, db, ctx, tenantID, "iv-fresh-a", 1000)
	planB := ivPlan(t, db, ctx, tenantID, "iv-fresh-b", 2000)
	planC := ivPlan(t, db, ctx, tenantID, "iv-fresh-c", 3000)
	planD := ivPlan(t, db, ctx, tenantID, "iv-fresh-d", 4000)
	store := NewPostgresStore(db)
	at := func(d time.Duration) context.Context { return clock.WithEffectiveNow(ctx, ivPS.Add(d)) }
	day := 24 * time.Hour

	// An active subscription that goes through every item writer.
	active := ivActiveSub(t, store, ctx, tenantID, cust, "sub-fresh-active", []domain.SubscriptionItem{
		{PlanID: planA.ID, Quantity: 1}, {PlanID: planB.ID, Quantity: 2},
	})
	added, err := store.AddItem(at(3*day), tenantID, domain.SubscriptionItem{SubscriptionID: active.ID, PlanID: planC.ID, Quantity: 1})
	if err != nil {
		t.Fatalf("add item: %v", err)
	}
	if _, err := store.UpdateItemQuantity(at(5*day), tenantID, active.Items[0].ID, 4); err != nil {
		t.Fatalf("quantity: %v", err)
	}
	if _, err := store.ApplyItemPlanImmediately(ctx, tenantID, active.Items[1].ID, planD.ID, ivPS.Add(7*day)); err != nil {
		t.Fatalf("plan change: %v", err)
	}
	if _, err := store.SetItemPendingPlan(ctx, tenantID, active.Items[0].ID, planB.ID, ivPE); err != nil {
		t.Fatalf("schedule plan change: %v", err)
	}
	if _, err := store.ApplyDuePendingItemPlansAtomic(ctx, tenantID, active.ID, ivPE, ivPE); err != nil {
		t.Fatalf("apply scheduled plan change: %v", err)
	}
	if err := store.RemoveItem(at(9*day), tenantID, added.ID); err != nil {
		t.Fatalf("remove item: %v", err)
	}

	// An active subscription canceled mid-period.
	canceled := ivActiveSub(t, store, ctx, tenantID, cust, "sub-fresh-canceled", []domain.SubscriptionItem{{PlanID: planA.ID, Quantity: 1}})
	if _, err := cancelNow(at(12*day), store, tenantID, canceled.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(ctx, `
		SELECT it.id, it.subscription_id, s.status, it.deleted_at IS NULL,
		       (SELECT count(*) FROM billing_intervals bi WHERE bi.subscription_item_id = it.id),
		       (SELECT count(*) FROM billing_intervals bi WHERE bi.subscription_item_id = it.id AND bi.ends_at IS NULL)
		FROM subscription_items it
		JOIN subscriptions s ON s.id = it.subscription_id
		ORDER BY it.subscription_id, it.created_at`)
	if err != nil {
		t.Fatalf("read items: %v", err)
	}
	defer func() { _ = rows.Close() }()
	items := 0
	for rows.Next() {
		var itemID, subID, status string
		var live bool
		var total, open int
		if err := rows.Scan(&itemID, &subID, &status, &live, &total, &open); err != nil {
			t.Fatalf("scan: %v", err)
		}
		items++
		switch {
		case total == 0:
			t.Errorf("item %s of subscription %s (%s) has a billable lifetime but no interval: a fresh database would need a backfill", itemID, subID, status)
		case status == string(domain.SubscriptionActive) && live && open != 1:
			t.Errorf("live item %s of active subscription %s has %d open intervals, want 1", itemID, subID, open)
		case (!live || status != string(domain.SubscriptionActive)) && open != 0:
			t.Errorf("item %s (live=%v) of %s subscription %s has %d open intervals, want 0", itemID, live, status, subID, open)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if items != 4 {
		t.Fatalf("read %d items, want 4 (the fixture changed; recount)", items)
	}
}
