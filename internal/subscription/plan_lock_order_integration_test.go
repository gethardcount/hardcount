package subscription

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// C10 (D08 §4.21): every subscription writer that references plans takes them
// in ascending id, in one statement, before its first write.
//
// The meter-in-use guard (D05) locks the meter, then every plan that lists it
// FOR UPDATE in ascending id. A subscription writer's foreign-key checks take
// FOR KEY SHARE on each plan it references, in the order its statements reach
// them. An immediate swap A→B reaches B first (the item's new plan_id), then A
// (the from_plan_id of the change row the item trigger writes). With
// id(A) < id(B), the guard holds A and waits for B while the swap holds B and
// waits for A, and Postgres aborts one of them with a deadlock error (40P01).
//
// The race is made deterministic with a plan edit (UPDATE plans, FOR NO KEY
// UPDATE) held open on B. It stops the guard after it has locked A and before
// it locks B, the window a real swap lands in. It does not stop the writer's
// FOR KEY SHARE on B, which that lock mode does not conflict with. When both
// wait, the plan edit rolls back.

// lockOrderPS and lockOrderPE bound every fixture subscription's period.
var (
	lockOrderPS = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	lockOrderPE = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
)

type lockOrderRig struct {
	ctx      context.Context
	db       *postgres.DB
	admin    *sql.DB
	tenantID string
	custID   string
	store    *PostgresStore
	pricing  *pricing.Service
}

func newLockOrderRig(t *testing.T) *lockOrderRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "plan lock order")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_lock_order", DisplayName: "Lock Order",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := NewPostgresStore(db)
	svc := pricing.NewService(pricing.NewPostgresStore(db))
	svc.SetSubscriptionPlanUsageReader(store)
	return &lockOrderRig{
		ctx: ctx, db: db, admin: testutil.AdminPool(t), tenantID: tenantID,
		custID: cust.ID, store: store, pricing: svc,
	}
}

// meterPlans creates a meter and two monthly plans that both list it. lo is
// the plan with the lower id in the database's order, hi the other.
func (r *lockOrderRig) meterPlans(t *testing.T, key string) (meterID string, lo, hi domain.Plan) {
	t.Helper()
	m, err := r.pricing.CreateMeter(r.ctx, r.tenantID, pricing.CreateMeterInput{
		Key: key, Name: key, Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter %s: %v", key, err)
	}
	ps := pricing.NewPostgresStore(r.db)
	var plans []domain.Plan
	for _, code := range []string{key + "-one", key + "-two"} {
		p, err := ps.CreatePlan(r.ctx, r.tenantID, domain.Plan{
			Code: code, Name: code, Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
			BaseAmountCents: 1000, Status: domain.PlanActive, MeterIDs: []string{m.ID},
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", code, err)
		}
		plans = append(plans, p)
	}
	var first string
	if err := r.admin.QueryRowContext(r.ctx,
		`SELECT id FROM plans WHERE id IN ($1, $2) ORDER BY id LIMIT 1`, plans[0].ID, plans[1].ID,
	).Scan(&first); err != nil {
		t.Fatalf("order plans: %v", err)
	}
	if first == plans[0].ID {
		return m.ID, plans[0], plans[1]
	}
	return m.ID, plans[1], plans[0]
}

// sub creates a subscription in the given status over lockOrderPS to
// lockOrderPE with one item per plan, created in the order given.
func (r *lockOrderRig) sub(t *testing.T, code string, status domain.SubscriptionStatus, plans ...domain.Plan) domain.Subscription {
	t.Helper()
	in := domain.Subscription{
		Code: code, DisplayName: code, CustomerID: r.custID,
		Status: status, BillingTime: domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &lockOrderPS,
		CurrentBillingPeriodEnd:   &lockOrderPE,
		NextBillingAt:             &lockOrderPE,
		Items:                     []domain.SubscriptionItem{{PlanID: plans[0].ID, Quantity: 1}},
	}
	switch status {
	case domain.SubscriptionActive:
		in.StartedAt = &lockOrderPS
	case domain.SubscriptionTrialing:
		in.TrialStartAt = &lockOrderPS
		in.TrialEndAt = &lockOrderPE
	}
	created, err := r.store.Create(clock.WithEffectiveNow(r.ctx, lockOrderPS), r.tenantID, in)
	if err != nil {
		t.Fatalf("create subscription %s: %v", code, err)
	}
	// Later items are added one hour apart, so the items' created_at order
	// (the order the activation writers open intervals in) is plans' order.
	for i, p := range plans[1:] {
		at := lockOrderPS.Add(time.Duration(i+1) * time.Hour)
		item, err := r.store.AddItem(clock.WithEffectiveNow(r.ctx, at), r.tenantID, domain.SubscriptionItem{
			SubscriptionID: created.ID, PlanID: p.ID, Quantity: 1,
		})
		if err != nil {
			t.Fatalf("add item on %s: %v", p.Code, err)
		}
		created.Items = append(created.Items, item)
	}
	return created
}

// holdPlan opens a transaction that holds plan id as a plan edit does
// (UPDATE plans, FOR NO KEY UPDATE). The caller rolls it back.
func (r *lockOrderRig) holdPlan(t *testing.T, id string) *sql.Tx {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(r.ctx, `UPDATE plans SET updated_at = updated_at WHERE id = $1`, id); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("hold plan: %v", err)
	}
	return tx
}

// waitBlockedBy returns once a backend in the test database waits on a lock
// held by a backend whose statement contains blockerLike.
func waitBlockedBy(t *testing.T, ctx context.Context, admin *sql.DB, blockerLike string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := admin.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity w
			WHERE w.datname = current_database() AND w.wait_event_type = 'Lock'
			  AND EXISTS (
			    SELECT 1 FROM pg_stat_activity b
			    WHERE b.pid = ANY(pg_blocking_pids(w.pid)) AND b.query ILIKE $1)
		`, "%"+blockerLike+"%").Scan(&n); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no backend blocked by a statement matching %q within 15s", blockerLike)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// d05Statement is text from CountSubsUsingMeterTx's plan lock.
const d05Statement = "meter_ids @>"

// lockOrderWriter is one subscription writer under test. setup builds its
// fixture on a fresh meter and returns the write, which must reference both
// plans. Most reach hi before lo when nothing orders their plan locks. The two
// downgrades reach lo first; they fail when a writer locks only some of its
// plans up front, such as the item's current one.
type lockOrderWriter struct {
	name  string
	setup func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(ctx context.Context) error
}

var lockOrderWriters = []lockOrderWriter{
	{"immediate plan change", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		sub := r.sub(t, "swap", domain.SubscriptionActive, lo)
		return func(ctx context.Context) error {
			_, err := r.store.ApplyItemPlanImmediately(ctx, r.tenantID, sub.Items[0].ID, hi.ID, lockOrderPS.Add(10*24*time.Hour))
			return err
		}
	}},
	// A downgrade reaches lo first. Its case fails if the swap locks only
	// the item's current plan.
	{"immediate plan change to the lower plan", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		sub := r.sub(t, "swap-down", domain.SubscriptionActive, hi)
		return func(ctx context.Context) error {
			_, err := r.store.ApplyItemPlanImmediately(ctx, r.tenantID, sub.Items[0].ID, lo.ID, lockOrderPS.Add(10*24*time.Hour))
			return err
		}
	}},
	{"scheduled plan change to the lower plan applied at the period end", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		sub := r.sub(t, "scheduled-down", domain.SubscriptionActive, hi)
		if _, err := r.store.SetItemPendingPlan(r.ctx, r.tenantID, sub.Items[0].ID, lo.ID, lockOrderPE); err != nil {
			t.Fatalf("schedule: %v", err)
		}
		return func(ctx context.Context) error {
			applied, err := r.store.ApplyDuePendingItemPlansAtomic(ctx, r.tenantID, sub.ID, lockOrderPE)
			if err == nil && len(applied) != 1 {
				t.Errorf("the scheduled change must apply to 1 item, applied %d", len(applied))
			}
			return err
		}
	}},
	{"scheduled plan change applied at the period end", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		sub := r.sub(t, "scheduled", domain.SubscriptionActive, lo)
		if _, err := r.store.SetItemPendingPlan(r.ctx, r.tenantID, sub.Items[0].ID, hi.ID, lockOrderPE); err != nil {
			t.Fatalf("schedule: %v", err)
		}
		return func(ctx context.Context) error {
			applied, err := r.store.ApplyDuePendingItemPlansAtomic(ctx, r.tenantID, sub.ID, lockOrderPE)
			if err == nil && len(applied) != 1 {
				t.Errorf("the scheduled change must apply to 1 item, applied %d", len(applied))
			}
			return err
		}
	}},
	{"create with two items", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := r.store.Create(clock.WithEffectiveNow(ctx, lockOrderPS), r.tenantID, domain.Subscription{
				Code: "create", DisplayName: "create", CustomerID: r.custID,
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				StartedAt:                 &lockOrderPS,
				CurrentBillingPeriodStart: &lockOrderPS,
				CurrentBillingPeriodEnd:   &lockOrderPE,
				NextBillingAt:             &lockOrderPE,
				Items:                     []domain.SubscriptionItem{{PlanID: hi.ID, Quantity: 1}, {PlanID: lo.ID, Quantity: 1}},
			})
			return err
		}
	}},
	{"draft activation", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		sub := r.sub(t, "draft", domain.SubscriptionDraft, hi, lo)
		return func(ctx context.Context) error {
			at := lockOrderPS.Add(2 * time.Hour)
			_, err := r.store.ActivateDraftWithBill(ctx, r.tenantID, sub.ID, at, at, lockOrderPE, 1, nil)
			return err
		}
	}},
	{"trial conversion", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		sub := r.sub(t, "trial", domain.SubscriptionTrialing, hi, lo)
		return func(ctx context.Context) error {
			_, err := r.store.ActivateAfterTrial(ctx, r.tenantID, sub.ID, lockOrderPS.Add(2*time.Hour))
			return err
		}
	}},
	{"early trial end", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		sub := r.sub(t, "trial-early", domain.SubscriptionTrialing, hi, lo)
		return func(ctx context.Context) error {
			at := lockOrderPS.Add(2 * time.Hour)
			_, err := r.store.EndTrialEarly(ctx, r.tenantID, sub.ID, at, at, lockOrderPE, lockOrderPE, 1)
			return err
		}
	}},
	{"billing-interval backfill", func(t *testing.T, r *lockOrderRig, lo, hi domain.Plan) func(context.Context) error {
		// A subscription that moved from hi to lo, with its intervals erased
		// as on a database from before billing_intervals: the backfill
		// writes the hi segment, then the lo one.
		sub := r.sub(t, "backfill", domain.SubscriptionActive, hi)
		if _, err := r.store.ApplyItemPlanImmediately(r.ctx, r.tenantID, sub.Items[0].ID, lo.ID, lockOrderPS.Add(5*24*time.Hour)); err != nil {
			t.Fatalf("swap: %v", err)
		}
		if _, err := r.admin.ExecContext(r.ctx, `DELETE FROM billing_intervals WHERE subscription_id = $1`, sub.ID); err != nil {
			t.Fatalf("erase intervals: %v", err)
		}
		return func(ctx context.Context) error {
			n, err := r.store.BackfillBillingIntervals(ctx)
			if err == nil && n != 2 {
				t.Errorf("the backfill must write 2 segments, wrote %d", n)
			}
			return err
		}
	}},
}

// TestPlanLockOrder_NoDeadlockWithMeterEdit is C10: each writer that
// references two plans, racing the D05 guard on a meter both plans list,
// finishes without a deadlock, and so does the guard.
func TestPlanLockOrder_NoDeadlockWithMeterEdit(t *testing.T) {
	r := newLockOrderRig(t)
	for i, w := range lockOrderWriters {
		t.Run(w.name, func(t *testing.T) {
			meterID, lo, hi := r.meterPlans(t, "lockord-"+string(rune('a'+i)))
			write := w.setup(t, r, lo, hi)

			hold := r.holdPlan(t, hi.ID)
			defer postgres.Rollback(hold)

			editDone := make(chan error, 1)
			go func() {
				agg := "max"
				_, err := r.pricing.UpdateMeter(r.ctx, r.tenantID, meterID, pricing.UpdateMeterInput{Aggregation: &agg})
				editDone <- err
			}()
			// The guard has locked lo and waits for hi.
			testutil.WaitForLockWaiter(t, r.ctx, r.admin, d05Statement)

			writeDone := make(chan error, 1)
			go func() { writeDone <- write(r.ctx) }()
			// The writer waits for lo, which the guard holds.
			waitBlockedBy(t, r.ctx, r.admin, d05Statement)

			if err := hold.Rollback(); err != nil {
				t.Fatalf("release the plan edit: %v", err)
			}

			var editErr, writeErr error
			for range 2 {
				select {
				case editErr = <-editDone:
				case writeErr = <-writeDone:
				case <-time.After(20 * time.Second):
					t.Fatal("the meter edit or the writer never finished")
				}
			}
			if postgres.IsDeadlock(editErr) || postgres.IsDeadlock(writeErr) {
				t.Fatalf("deadlock (40P01): meter edit err=%v, writer err=%v", editErr, writeErr)
			}
			if writeErr != nil {
				t.Fatalf("writer: %v", writeErr)
			}
			if editErr != nil && errs.Code(editErr) != errs.CodeMeterInUse {
				t.Fatalf("meter edit: want success or meter_in_use, got %v", editErr)
			}
		})
	}
}

// TestPlanLockOrder_LockStatementIsAscending pins the order inside the lock
// statement itself, whatever scan the planner picks. Postgres usually reads
// `id = ANY($1)` through the primary key, which returns ids in order anyway,
// so the race test above would not notice a statement that lost its ORDER BY.
// Here the transaction scans the table sequentially, and lo's live row is
// moved after hi's: without the ORDER BY the statement locks hi, then waits
// for lo.
func TestPlanLockOrder_LockStatementIsAscending(t *testing.T) {
	r := newLockOrderRig(t)
	_, lo, hi := r.meterPlans(t, "lockasc")
	// An update writes lo's new row version after hi's in the heap.
	if _, err := r.admin.ExecContext(r.ctx, `UPDATE plans SET updated_at = updated_at WHERE id = $1`, lo.ID); err != nil {
		t.Fatalf("move lo: %v", err)
	}

	guard, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin guard: %v", err)
	}
	defer postgres.Rollback(guard)
	if _, err := guard.ExecContext(r.ctx, `SELECT id FROM plans WHERE id = $1 FOR UPDATE`, lo.ID); err != nil {
		t.Fatalf("guard locks lo: %v", err)
	}

	writer, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer postgres.Rollback(writer)
	if _, err := writer.ExecContext(r.ctx, `SET LOCAL enable_indexscan = off; SET LOCAL enable_bitmapscan = off`); err != nil {
		t.Fatalf("force a sequential scan: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- lockPlansTx(r.ctx, writer, []string{hi.ID, lo.ID}) }()
	waitBlockedBy(t, r.ctx, r.admin, "WHERE id = $1 FOR UPDATE")

	// The writer waits for lo, so it must not hold hi yet: an exclusive lock
	// on hi is free.
	probe, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	defer postgres.Rollback(probe)
	if _, err := probe.ExecContext(r.ctx, `SELECT id FROM plans WHERE id = $1 FOR UPDATE NOWAIT`, hi.ID); err != nil {
		t.Errorf("the lock statement took hi before lo: %v", err)
	}
	postgres.Rollback(probe)

	if err := guard.Rollback(); err != nil {
		t.Fatalf("release lo: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("lock plans: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the lock statement never finished")
	}
}

// TestPlanLockOrder_SinglePlanWritersLockFirst covers the writers that
// reference one plan. One lock alone cannot deadlock against the guard, so
// the test pins where it is taken: with the plan held FOR UPDATE, as the
// guard holds it, the writer waits in its plan-lock statement, before any
// write that references the plan.
func TestPlanLockOrder_SinglePlanWritersLockFirst(t *testing.T) {
	r := newLockOrderRig(t)
	cases := []struct {
		name  string
		write func(ctx context.Context, sub domain.Subscription, plan domain.Plan) error
	}{
		{"item add", func(ctx context.Context, sub domain.Subscription, plan domain.Plan) error {
			_, err := r.store.AddItem(clock.WithEffectiveNow(ctx, lockOrderPS.Add(time.Hour)), r.tenantID, domain.SubscriptionItem{
				SubscriptionID: sub.ID, PlanID: plan.ID, Quantity: 1,
			})
			return err
		}},
		{"scheduled plan change", func(ctx context.Context, sub domain.Subscription, plan domain.Plan) error {
			_, err := r.store.SetItemPendingPlan(ctx, r.tenantID, sub.Items[0].ID, plan.ID, lockOrderPE)
			return err
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, lo, hi := r.meterPlans(t, "lockone-"+string(rune('a'+i)))
			sub := r.sub(t, "one-"+string(rune('a'+i)), domain.SubscriptionActive, lo)

			hold, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer postgres.Rollback(hold)
			if _, err := hold.ExecContext(r.ctx, `SELECT id FROM plans WHERE id = $1 FOR UPDATE`, hi.ID); err != nil {
				t.Fatalf("hold plan: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- c.write(r.ctx, sub, hi) }()
			testutil.WaitForLockWaiter(t, r.ctx, r.admin, "ORDER BY id FOR KEY SHARE")

			if err := hold.Rollback(); err != nil {
				t.Fatalf("release plan: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("writer: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the writer never finished")
			}
		})
	}
}
