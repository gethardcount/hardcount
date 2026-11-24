package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/walk"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seedSweepInvoice mutates a fresh draft invoice into the exact state a sweep
// scans for, in its own tenant (seedDraftInvoice pins a fixed customer
// external_id, so each row needs its own tenant; the sweeps scan cross-tenant
// under TxBypass, so one scan sees them all).
func seedSweepInvoice(t *testing.T, ctx context.Context, db *postgres.DB, name, sql string) string {
	t.Helper()
	tenantID := testutil.CreateTestTenant(t, db, name)
	id := seedDraftInvoice(t, db, tenantID)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, sql, id); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	return id
}

// A failed invoice with no dunning run, past the cool-off, is listed for the
// dunning backfill: one-offs (no subscription) included.
func TestListFailedWithoutDunningRun_ListsFailedInvoicesWithNoRun(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)

	// updated_at is set two hours in the past so both rows satisfy the sweep's
	// `updated_at < olderThan` grace window (olderThan = now, below).
	oneOff := seedSweepInvoice(t, ctx, db, "Dunning Enroll One-Off", `
		UPDATE invoices SET subscription_id = NULL, payment_status = 'failed',
		       status = 'finalized', amount_due_cents = 1000,
		       updated_at = now() - interval '2 hours'
		 WHERE id = $1`)
	cycle := seedSweepInvoice(t, ctx, db, "Dunning Enroll Cycle", `
		UPDATE invoices SET payment_status = 'failed', status = 'finalized',
		       amount_due_cents = 1000,
		       updated_at = now() - interval '2 hours'
		 WHERE id = $1`)

	failed, err := store.ListFailedWithoutDunningRun(ctx, time.Now(), 200)
	if err != nil {
		t.Fatalf("ListFailedWithoutDunningRun: %v", err)
	}
	got := map[string]bool{}
	for _, inv := range failed {
		got[inv.ID] = true
	}
	if !got[oneOff] || !got[cycle] {
		t.Errorf("listed %v, want both the one-off %s and the cycle invoice %s", got, oneOff, cycle)
	}
}

// TestListAutoChargePending_ExcludesPausedSubscription pins that collection
// honours `pause_collection`. A paused subscription is one the operator — or
// dunning's `pause` final action — took off automatic collection, so the sweep
// must not charge its invoices.
//
// This used to hold by accident: every finalize site skips collection for a
// paused sub, so nothing ever set auto_charge_pending on one and the sweep
// never saw it. That stopped being true once the tax-retry chain began queueing
// the invoices it auto-finalizes — a paused sub's tax-deferred draft finalizes
// on retry, and without this predicate the very next sweep would charge it.
func TestListAutoChargePending_ExcludesPausedSubscription(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	store := invoice.NewPostgresStore(db)

	paused := seedSweepInvoice(t, ctx, db, "AutoCharge Paused Sub", `
		UPDATE invoices SET auto_charge_pending = TRUE, payment_status = 'pending',
		       status = 'finalized', amount_due_cents = 1000,
		       updated_at = now()
		 WHERE id = $1`)
	pauseSubscriptionOf(t, ctx, db, paused)

	running := seedSweepInvoice(t, ctx, db, "AutoCharge Running Sub", `
		UPDATE invoices SET auto_charge_pending = TRUE, payment_status = 'pending',
		       status = 'finalized', amount_due_cents = 1000,
		       updated_at = now()
		 WHERE id = $1`)

	listed, err := store.ListAutoChargePending(ctx, walk.Key{}, 100)
	if err != nil {
		t.Fatalf("ListAutoChargePending: %v", err)
	}
	var sawPaused, sawRunning bool
	for _, inv := range listed {
		switch inv.ID {
		case paused:
			sawPaused = true
		case running:
			sawRunning = true
		}
	}
	if sawPaused {
		t.Error("a paused subscription's invoice was listed for auto-charge — collection must respect pause_collection")
	}
	if !sawRunning {
		t.Error("a running subscription's invoice must still be listed (the pause predicate over-filtered)")
	}
}

// pauseSubscriptionOf sets pause_collection on the subscription owning the
// given invoice, mirroring what dunning's `pause` final action persists.
func pauseSubscriptionOf(t *testing.T, ctx context.Context, db *postgres.DB, invoiceID string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin pause tx: %v", err)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft'
		 WHERE id = (SELECT subscription_id FROM invoices WHERE id = $1)`, invoiceID)
	if err != nil {
		t.Fatalf("pause subscription: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("fixture expectation: invoice %s must own exactly one subscription to pause, updated %d", invoiceID, n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit pause tx: %v", err)
	}
}
