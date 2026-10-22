package subscription

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A canceled subscription carries no cancel schedule and no pending plan
// change on its items: both say what a live subscription does next. Every
// cancel writer used to leave them, so the API and the dashboard showed a
// scheduled cancel (with Undo) or a coming plan change on a canceled
// subscription. The collection pause stays: it still holds the
// subscription's unpaid invoices until an operator resumes collection.

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
// canceled with no schedule and no pending plan change, and keeps its pause.
func (r *liveStateRig) requireRetired(label string, sub domain.Subscription) {
	r.t.Helper()
	if sub.Status != domain.SubscriptionCanceled {
		r.t.Fatalf("%s: status %s, want canceled", label, sub.Status)
	}
	if sub.CancelAt != nil || sub.CancelAtPeriodEnd {
		r.t.Errorf("%s: cancel schedule left on a canceled subscription: cancel_at=%v at_period_end=%v", label, sub.CancelAt, sub.CancelAtPeriodEnd)
	}
	if sub.PauseCollection == nil {
		r.t.Errorf("%s: the cancel cleared the pause: it still holds the subscription's unpaid invoices", label)
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
// change. Each must hand back, and store, a row with no schedule and no
// pending plan change but with its pause, without writing an item-change
// row (clearing the pending columns is no plan or quantity change). A writer
// that runs billFn must give it the row with its pause: the final invoice is
// decided against the pause in force at the cancel instant.
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

// TestMigration_ClearsLiveOnlyStateOnCanceledSubs: every cancel before this
// release left its schedule and pending plan change on the row. The
// migration clears them on canceled and archived subscriptions, keeps their
// pause, and leaves a live one untouched.
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
		if got.CancelAtPeriodEnd || got.CancelAt != nil || got.Items[0].PendingPlanID != "" || got.Items[0].PendingPlanEffectiveAt != nil {
			t.Errorf("%s (%s) keeps live-only state: at_period_end=%v cancel_at=%v pending=%q",
				id, got.Status, got.CancelAtPeriodEnd, got.CancelAt, got.Items[0].PendingPlanID)
		}
		if got.PauseCollection == nil {
			t.Errorf("%s (%s): the migration cleared the pause, which still holds its unpaid invoices", id, got.Status)
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
