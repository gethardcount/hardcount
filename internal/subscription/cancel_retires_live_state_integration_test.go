package subscription

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// A canceled subscription carries none of the state that says what a live
// subscription does next: no cancel schedule, no collection pause, no
// pending plan change on its items. Every cancel writer used to leave some
// of it. The rows then lied: the API and the dashboard showed a scheduled
// cancel or a paused collection (with Undo and Resume) on a canceled
// subscription, and the pause-resume sweep announced a resume for it.

// liveStateRig is one tenant with two plans, for subscriptions that carry a
// cancel schedule, a keep_as_draft pause with resumes_at, and a pending plan
// change on their item.
type liveStateRig struct {
	t        *testing.T
	ctx      context.Context
	db       *postgres.DB
	store    *PostgresStore
	tenantID string
	custID   string
	planA    domain.Plan
	planB    domain.Plan
	now      time.Time
}

func newLiveStateRig(t *testing.T, name string) *liveStateRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	return &liveStateRig{
		t: t, ctx: ctx, db: db, store: NewPostgresStore(db), tenantID: tenantID,
		custID: ivCustomer(t, db, ctx, tenantID, "cus_live_state"),
		planA:  ivPlan(t, db, ctx, tenantID, "live-state-a", 1000),
		planB:  ivPlan(t, db, ctx, tenantID, "live-state-b", 2000),
		now:    time.Now().UTC().Truncate(time.Second),
	}
}

// seed creates a subscription in status (active or trialing) with a pause and
// a pending plan change. The caller schedules its cancel.
func (r *liveStateRig) seed(code string, status domain.SubscriptionStatus) domain.Subscription {
	r.t.Helper()
	start := r.now.Add(-24 * time.Hour)
	end := start.AddDate(0, 1, 0)
	in := domain.Subscription{
		Code: code, DisplayName: code, CustomerID: r.custID,
		Status: status, BillingTime: domain.BillingTimeCalendar,
		Items: []domain.SubscriptionItem{{PlanID: r.planA.ID, Quantity: 1}},
	}
	if status == domain.SubscriptionTrialing {
		trialEnd := r.now.Add(7 * 24 * time.Hour)
		in.TrialStartAt, in.TrialEndAt = &start, &trialEnd
	} else {
		in.StartedAt, in.CurrentBillingPeriodStart, in.CurrentBillingPeriodEnd, in.NextBillingAt = &start, &start, &end, &end
	}
	sub, err := r.store.Create(r.ctx, r.tenantID, in)
	if err != nil {
		r.t.Fatalf("create %s: %v", code, err)
	}
	resumes := r.now.Add(10 * 24 * time.Hour)
	if _, err := r.store.SetPauseCollection(r.ctx, r.tenantID, sub.ID, domain.PauseCollection{
		Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumes,
	}); err != nil {
		r.t.Fatalf("pause %s: %v", code, err)
	}
	if _, err := r.store.SetItemPendingPlan(r.ctx, r.tenantID, sub.Items[0].ID, r.planB.ID, end); err != nil {
		r.t.Fatalf("pending plan %s: %v", code, err)
	}
	if sub, err = r.store.Get(r.ctx, r.tenantID, sub.ID); err != nil {
		r.t.Fatalf("re-read %s: %v", code, err)
	}
	if sub.PauseCollection == nil || sub.Items[0].PendingPlanID != r.planB.ID {
		r.t.Fatalf("seed %s: pause=%v pending=%q, want both set", code, sub.PauseCollection, sub.Items[0].PendingPlanID)
	}
	return sub
}

func (r *liveStateRig) schedule(sub domain.Subscription, at *time.Time, atPeriodEnd bool) {
	r.t.Helper()
	if _, err := r.store.ScheduleCancellation(r.ctx, r.tenantID, sub.ID, at, atPeriodEnd, sub.Status); err != nil {
		r.t.Fatalf("schedule cancel: %v", err)
	}
}

func (r *liveStateRig) itemChanges(subID string) int {
	r.t.Helper()
	var n int
	if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.ctx, `SELECT count(*) FROM subscription_item_changes WHERE subscription_id = $1`, subID).Scan(&n)
	}); err != nil {
		r.t.Fatalf("count item changes: %v", err)
	}
	return n
}

// requireRetired fails unless sub (the returned and the stored row) is
// canceled with no schedule, no pause and no pending plan change.
func (r *liveStateRig) requireRetired(label string, sub domain.Subscription) {
	r.t.Helper()
	if sub.Status != domain.SubscriptionCanceled {
		r.t.Fatalf("%s: status %s, want canceled", label, sub.Status)
	}
	if sub.CancelAt != nil || sub.CancelAtPeriodEnd {
		r.t.Errorf("%s: cancel schedule left on a canceled subscription: cancel_at=%v at_period_end=%v", label, sub.CancelAt, sub.CancelAtPeriodEnd)
	}
	if sub.PauseCollection != nil {
		r.t.Errorf("%s: pause left on a canceled subscription: %+v", label, *sub.PauseCollection)
	}
	if len(sub.Items) == 0 {
		r.t.Fatalf("%s: no items", label)
	}
	for _, it := range sub.Items {
		if it.PendingPlanID != "" || it.PendingPlanEffectiveAt != nil {
			r.t.Errorf("%s: item %s keeps pending plan %q at %v", label, it.ID, it.PendingPlanID, it.PendingPlanEffectiveAt)
		}
	}
}

// TestCancelWriters_RetireLiveOnlyState runs each of the four cancel writers
// on a subscription with a cancel schedule, a pause and a pending plan
// change. Each must hand back, and store, a row with none of them, without
// writing an item-change row (clearing the pending columns is no plan or
// quantity change). A writer that runs billFn must give it the row as it was
// at the cancel instant, pause included: the final invoice is decided from
// it.
//
// Mutation: drop retireLiveOnlyStateTx from any one writer and its case
// fails; drop its items UPDATE and every case fails.
func TestCancelWriters_RetireLiveOnlyState(t *testing.T) {
	r := newLiveStateRig(t, "Cancel Retires Live State")

	type writer struct {
		name   string
		status domain.SubscriptionStatus
		cancel func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription)
	}
	billSaw := func(saw **domain.Subscription) func(tx *sql.Tx, c domain.Subscription) error {
		return func(_ *sql.Tx, c domain.Subscription) error {
			cp := c
			*saw = &cp
			return nil
		}
	}
	writers := []writer{
		{"CancelAtomicWithBill", domain.SubscriptionActive, func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription) {
			r.schedule(sub, nil, true)
			var saw *domain.Subscription
			got, err := r.store.CancelAtomicWithBill(r.ctx, r.tenantID, sub.ID, billSaw(&saw))
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			return got, saw
		}},
		{"FireScheduledCancellationWithBill", domain.SubscriptionActive, func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription) {
			at := r.now.Add(-time.Minute)
			r.schedule(sub, &at, false)
			var saw *domain.Subscription
			got, err := r.store.FireScheduledCancellationWithBill(r.ctx, r.tenantID, sub.ID, at, billSaw(&saw))
			if err != nil {
				t.Fatalf("fire: %v", err)
			}
			return got, saw
		}},
		{"FireScheduledCancellationTx", domain.SubscriptionActive, func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription) {
			r.schedule(sub, nil, true)
			var got domain.Subscription
			if err := r.store.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
				var err error
				got, err = r.store.FireScheduledCancellationTx(r.ctx, tx, r.tenantID, sub.ID, SnapshotOf(sub), r.now)
				return err
			}); err != nil {
				t.Fatalf("fire at period end: %v", err)
			}
			return got, nil
		}},
		{"CancelAtTrialEnd", domain.SubscriptionTrialing, func(t *testing.T, sub domain.Subscription) (domain.Subscription, *domain.Subscription) {
			r.schedule(sub, nil, true)
			got, err := r.store.CancelAtTrialEnd(r.ctx, r.tenantID, sub.ID, *sub.TrialEndAt)
			if err != nil {
				t.Fatalf("cancel at trial end: %v", err)
			}
			return got, nil
		}},
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			sub := r.seed("sub-live-state-"+strings.ToLower(w.name), w.status)
			changesBefore := r.itemChanges(sub.ID)

			got, billInput := w.cancel(t, sub)

			if billInput != nil && billInput.PauseCollection == nil {
				t.Error("billFn got the row without its pause: the final invoice must be decided against the pause in force at the cancel instant")
			}
			r.requireRetired("returned row", got)
			stored, err := r.store.Get(r.ctx, r.tenantID, sub.ID)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			r.requireRetired("stored row", stored)
			if after := r.itemChanges(sub.ID); after != changesBefore {
				t.Errorf("item-change rows %d → %d: clearing a pending plan is not a plan change", changesBefore, after)
			}
		})
	}
}

// TestProcessExpiredPauseCollections_CanceledSubAnnouncesNothing: a paused
// subscription is canceled, then its pause's resumes_at passes. The sweep
// used to pick it up (its status filter only holds back live subscriptions
// that still owe a close) and wrote a collection_resumed audit row and sent
// subscription.collection_resumed for a subscription that had ended. The
// cancel now clears the pause, so neither sweep finds it.
func TestProcessExpiredPauseCollections_CanceledSubAnnouncesNothing(t *testing.T) {
	r := newLiveStateRig(t, "Canceled Pause Sweep")
	logger := audit.NewLogger(r.db)
	outbox := webhook.NewOutboxStore(r.db)

	newSvc := func(now time.Time) *Service {
		svc := NewService(r.store, clock.NewFake(now))
		svc.SetAuditLogger(logger)
		svc.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
		return svc
	}
	cancelPaused := func(t *testing.T, code string) domain.Subscription {
		t.Helper()
		sub := r.seed(code, domain.SubscriptionActive)
		if _, err := r.store.CancelAtomicWithBill(r.ctx, r.tenantID, sub.ID, nil); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		return sub
	}
	requireSilent := func(t *testing.T, sub domain.Subscription, processed int, errs []error) {
		t.Helper()
		if len(errs) != 0 {
			t.Fatalf("sweep errors: %v", errs)
		}
		if processed != 0 {
			t.Errorf("sweep resumed %d subscriptions, want 0: the only paused one was canceled", processed)
		}
		rows, _, err := logger.Query(r.ctx, r.tenantID, audit.QueryFilter{ResourceType: "subscription", ResourceID: sub.ID})
		if err != nil {
			t.Fatalf("query audit: %v", err)
		}
		for _, row := range rows {
			if row.Metadata["action"] == domain.SubAuditCollectionResumed {
				t.Errorf("collection_resumed audit row written for canceled subscription %s", sub.ID)
			}
		}
		var events int
		if err := r.db.WithTenantTx(r.ctx, r.tenantID, func(tx *sql.Tx) error {
			return tx.QueryRowContext(r.ctx, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1 AND payload->>'subscription_id' = $2`,
				domain.EventSubscriptionCollectionResumed, sub.ID).Scan(&events)
		}); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		if events != 0 {
			t.Errorf("%d subscription.collection_resumed events queued for canceled subscription %s, want 0", events, sub.ID)
		}
	}
	past := r.now.Add(11 * 24 * time.Hour) // past the seed's resumes_at

	t.Run("wall clock", func(t *testing.T) {
		sub := cancelPaused(t, "sub-pause-sweep-wall")
		processed, errs := newSvc(past).ProcessExpiredPauseCollections(r.ctx, 100)
		requireSilent(t, sub, processed, errs)
	})

	t.Run("test clock", func(t *testing.T) {
		const clockID = "vlx_tclk_canceled_pause"
		sub := cancelPaused(t, "sub-pause-sweep-clock")
		tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		for _, q := range []string{
			`SELECT set_config('app.livemode', 'off', true)`,
			`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode) VALUES ('` + clockID + `', '` + r.tenantID + `', 'canceled pause', now(), 'ready', false)`,
			`UPDATE subscriptions SET test_clock_id = '` + clockID + `' WHERE id = '` + sub.ID + `'`,
		} {
			if _, err := tx.ExecContext(r.ctx, q); err != nil {
				t.Fatalf("seed clock: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		processed, errs := newSvc(past).ProcessExpiredPauseCollectionsForClock(r.ctx, r.tenantID, clockID, past)
		requireSilent(t, sub, processed, errs)
	})
}

// TestMigration_ClearsLiveOnlyStateOnCanceledSubs: every cancel before this
// release left its schedule, pause and pending plan change on the row, and
// the pause-resume sweep still acts on such rows. The migration clears them
// on canceled and archived subscriptions and leaves a live one untouched.
func TestMigration_ClearsLiveOnlyStateOnCanceledSubs(t *testing.T) {
	r := newLiveStateRig(t, "Migration Clears Live State")
	canceled := r.seed("sub-mig-canceled", domain.SubscriptionActive)
	archived := r.seed("sub-mig-archived", domain.SubscriptionActive)
	live := r.seed("sub-mig-live", domain.SubscriptionActive)
	r.schedule(live, nil, true)

	// The rows an earlier cancel left: terminal status, every field kept.
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, `UPDATE subscriptions SET status = CASE id WHEN $1 THEN 'canceled' ELSE 'archived' END,
		canceled_at = now(), cancel_at_period_end = true WHERE id IN ($1, $2)`, canceled.ID, archived.ID); err != nil {
		t.Fatalf("seed terminal rows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	applyMigrationUp(t, r.ctx, r.db, "clear_canceled_subscription_live_state")

	for _, id := range []string{canceled.ID, archived.ID} {
		got, err := r.store.Get(r.ctx, r.tenantID, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.CancelAtPeriodEnd || got.CancelAt != nil || got.PauseCollection != nil || got.Items[0].PendingPlanID != "" || got.Items[0].PendingPlanEffectiveAt != nil {
			t.Errorf("%s (%s) keeps live-only state: at_period_end=%v cancel_at=%v pause=%v pending=%q",
				id, got.Status, got.CancelAtPeriodEnd, got.CancelAt, got.PauseCollection, got.Items[0].PendingPlanID)
		}
	}
	got, err := r.store.Get(r.ctx, r.tenantID, live.ID)
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if !got.CancelAtPeriodEnd || got.PauseCollection == nil || got.Items[0].PendingPlanID != r.planB.ID {
		t.Errorf("the migration changed a live subscription: at_period_end=%v pause=%v pending=%q", got.CancelAtPeriodEnd, got.PauseCollection, got.Items[0].PendingPlanID)
	}
}

// applyMigrationUp runs the Up section of the migration *_<suffix>.sql on
// rows the test seeded after the test database was migrated. It runs as the
// app role in a plain transaction (no tenant, no bypass), a user row-level
// security applies to, so the file's own bypass line is what lets it see the
// rows. A check first proves the transaction sees no subscription without it.
func applyMigrationUp(t *testing.T, ctx context.Context, db *postgres.DB, suffix string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "platform", "migrate", "sql", "*_"+suffix+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("migration *_%s.sql: found %d files, want 1", suffix, len(files))
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	up, _, found := strings.Cut(string(body), "-- +goose Down")
	if !found {
		t.Fatalf("%s: no Down section", files[0])
	}
	up = strings.Replace(up, "-- +goose Up", "", 1)
	tx, err := db.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var visible int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM subscriptions`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Fatalf("setup: the migration's transaction sees %d subscriptions before its own bypass line; it must run as a user row-level security applies to", visible)
	}
	if _, err := tx.ExecContext(ctx, up); err != nil {
		t.Fatalf("%s: %v", files[0], err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
