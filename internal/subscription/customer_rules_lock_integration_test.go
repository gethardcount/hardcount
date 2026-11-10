package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// custLockAt is the instant of every write in the customer-rules lock tests:
// inside the fixture period [lockOrderPS, lockOrderPE).
var custLockAt = lockOrderPS.Add(9 * 24 * time.Hour)

// TestCustomerLock_MeterWritersSerialize: a write that puts a meter on one of
// a customer's subscriptions and a create of another subscription on the same
// meter, at the same moment, never both commit. Usage is summed per customer
// and meter, so two live items on one meter bill its usage twice every period.
//
// The first writer is an item add, an immediate plan change or a scheduled
// plan change, run on a test-owned transaction that waits before it commits.
// The racer is a create on another plan that bills the same meter. Neither
// sees the other's uncommitted item in its meter check unless something
// orders them: the customer-rules lock, which the item writer takes in
// LockForItemChangeTx and the create takes before its insert. The create
// then waits, reads the committed item and is refused.
//
// Mutation-verify: drop the lock from LockForItemChangeTx, or from createInTx
// → each case ends with two live items on the meter. Take it in
// applyItemPlanImmediatelyInTx instead of LockForItemChangeTx → the add and
// the scheduled change fail.
func TestCustomerLock_MeterWritersSerialize(t *testing.T) {
	r := newLockOrderRig(t)
	svc := NewService(r.store, clock.NewFake(custLockAt))
	svc.SetPlanReader(r.pricing)
	svc.SetPlanMeterReader(r.pricing)
	ctx := clock.WithEffectiveNow(r.ctx, custLockAt)

	writers := []struct {
		name string
		// write puts plan p on first, on tx: a new item, or first's only
		// item moved to p.
		write func(tx *sql.Tx, first domain.Subscription, p domain.Plan) error
	}{
		{"add", func(tx *sql.Tx, first domain.Subscription, p domain.Plan) error {
			_, err := svc.AddItemTx(ctx, tx, r.tenantID, first.ID, AddItemInput{PlanID: p.ID})
			return err
		}},
		{"immediate swap", func(tx *sql.Tx, first domain.Subscription, p domain.Plan) error {
			_, err := svc.UpdateItemTx(ctx, tx, r.tenantID, first.ID, first.Items[0].ID, UpdateItemInput{NewPlanID: p.ID, Immediate: true})
			return err
		}},
		{"scheduled swap", func(tx *sql.Tx, first domain.Subscription, p domain.Plan) error {
			_, err := svc.UpdateItemTx(ctx, tx, r.tenantID, first.ID, first.Items[0].ID, UpdateItemInput{NewPlanID: p.ID})
			return err
		}},
	}
	for i, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			key := fmt.Sprintf("custlock-%c", 'a'+i)
			meterID := "mtr_" + strings.ReplaceAll(key, "-", "_")
			plan := func(code string, meters ...string) domain.Plan {
				return createPlanPricing(t, r.db, r.ctx, r.tenantID, domain.Plan{
					Code: code, Name: code, Currency: "USD",
					BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
					BaseAmountCents: 1000, Status: domain.PlanActive,
				}, meters...)
			}
			base := plan(key + "-base")
			p := plan(key+"-p", meterID)
			q := plan(key+"-q", meterID)
			first := r.sub(t, key+"-first", domain.SubscriptionActive, base)

			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			defer releaseOnce()
			holding := make(chan struct{})
			writeDone := make(chan error, 1)
			go func() {
				writeDone <- r.store.WithTenantTx(ctx, r.tenantID, func(tx *sql.Tx) error {
					if err := w.write(tx, first, p); err != nil {
						return err
					}
					close(holding)
					<-release
					return nil
				})
			}()
			select {
			case <-holding:
			case err := <-writeDone:
				t.Fatalf("the %s failed before it held its transaction open: %v", w.name, err)
			case <-time.After(15 * time.Second):
				t.Fatalf("the %s never reached its commit", w.name)
			}

			racerDone := make(chan struct{})
			var racerErr error
			go func() {
				defer close(racerDone)
				_, racerErr = r.store.Create(ctx, r.tenantID, domain.Subscription{
					Code: key + "-racer", DisplayName: key + "-racer", CustomerID: r.custID,
					Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
					StartedAt:                 &lockOrderPS,
					CurrentBillingPeriodStart: &lockOrderPS,
					CurrentBillingPeriodEnd:   &lockOrderPE,
					NextBillingAt:             &lockOrderPE,
					Items:                     []domain.SubscriptionItem{{PlanID: q.ID, Quantity: 1}},
				})
			}()
			// The create waits for the customer-rules lock the writer holds,
			// or, with nothing ordering them, commits at once.
			testutil.WaitForLockWaitersOrDone(t, r.ctx, r.admin, "pg_advisory_xact_lock", 1, racerDone)
			releaseOnce()

			var writeErr error
			select {
			case writeErr = <-writeDone:
			case <-time.After(20 * time.Second):
				t.Fatalf("the %s never committed", w.name)
			}
			select {
			case <-racerDone:
			case <-time.After(20 * time.Second):
				t.Fatal("the create never finished")
			}

			if n := liveItemsOnMeter(t, r, meterID); n != 1 {
				t.Fatalf("%d live items bill meter %s for the customer, want 1: the %s (err=%v) and the create (err=%v) both passed the meter check, so its usage is billed %d times", n, meterID, w.name, writeErr, racerErr, n)
			}
			if writeErr != nil {
				t.Fatalf("the %s, which held the lock first: %v", w.name, writeErr)
			}
			if !errors.Is(racerErr, errs.ErrInvalidState) || !strings.Contains(racerErr.Error(), meterID) {
				t.Fatalf("the create = %v, want InvalidState naming meter %s", racerErr, meterID)
			}
		})
	}
}

// liveItemsOnMeter counts the customer's live subscription items whose plan
// or pending plan bills meterID: the claims the meter check counts.
func liveItemsOnMeter(t *testing.T, r *lockOrderRig, meterID string) int {
	t.Helper()
	var n int
	if err := r.admin.QueryRowContext(context.Background(), `
		SELECT count(DISTINCT i.id) FROM subscription_items i
		JOIN subscriptions s ON s.id = i.subscription_id
		JOIN plan_prices pp ON pp.plan_id = i.plan_id OR pp.plan_id = i.pending_plan_id
		WHERE s.customer_id = $1 AND s.status IN ('draft', 'trialing', 'active')
		  AND i.deleted_at IS NULL AND pp.meter_id = $2`, r.custID, meterID).Scan(&n); err != nil {
		t.Fatalf("count items on meter %s: %v", meterID, err)
	}
	return n
}
