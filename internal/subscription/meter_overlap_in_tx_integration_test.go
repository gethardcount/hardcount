package subscription

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/pricing"
)

// The service's meter-overlap check (rejectMeterOverlap) reads the plans'
// meters before the write's transaction. A price for a new meter can commit
// in between: on a plan no one has used it runs no overlap check, and on a
// used plan its check cannot see the item that is not written yet. So every
// writer that puts a plan on an item checks again on its transaction, after
// it holds the plans FOR KEY SHARE. A price write holds its plan FOR UPDATE,
// so one that commits first is seen by that check, and one that comes later
// waits for the item and then refuses the meter (409 meter_overlap).
//
// The test plays the interleaving: the service's check has passed, then the
// price commits, then the store write runs.
//
// Mutation: drop the store's check, and each write commits a second live
// item that prices the meter.
func TestStore_MeterOverlapCheckedOnTheWritesTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	r := newLockOrderRig(t)
	at := clock.WithEffectiveNow(r.ctx, lockOrderPS)

	meterID := "mtr_overlap_in_tx"
	planA := createPlanPricing(t, r.db, r.ctx, r.tenantID, domain.Plan{
		Code: "overlap-a", Name: "overlap-a", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
		BaseAmountCents: 1000, Status: domain.PlanActive,
	}, meterID)
	planQ := createPlanPricing(t, r.db, r.ctx, r.tenantID, domain.Plan{
		Code: "overlap-q", Name: "overlap-q", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
		BaseAmountCents: 1000, Status: domain.PlanActive,
	})
	planP := createPlanPricing(t, r.db, r.ctx, r.tenantID, domain.Plan{
		Code: "overlap-p", Name: "overlap-p", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
		BaseAmountCents: 1000, Status: domain.PlanActive,
	})
	r.sub(t, "overlap-on-a", domain.SubscriptionActive, planA)
	// One subscription on plan Q, which prices nothing, for each write that
	// changes an existing subscription.
	onQ := map[string]domain.Subscription{}
	for _, name := range []string{"add", "swap", "schedule"} {
		onQ[name] = r.sub(t, "overlap-on-q-"+name, domain.SubscriptionActive, planQ)
	}

	// The service's check has passed for each write below: plan P priced no
	// meter. Now P prices the customer's meter. P is never-used, so the add
	// runs no overlap check.
	if _, err := r.pricing.AddPrice(r.ctx, r.tenantID, planP.ID, pricing.PriceInput{
		MeterID:    meterID,
		PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)},
	}); err != nil {
		t.Fatalf("add price on plan P: %v", err)
	}

	for _, w := range []struct {
		name  string
		write func() error
	}{
		{"create", func() error {
			_, err := r.store.Create(at, r.tenantID, domain.Subscription{
				Code: "overlap-on-p", DisplayName: "overlap-on-p", CustomerID: r.custID,
				Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
				StartedAt:                 &lockOrderPS,
				CurrentBillingPeriodStart: &lockOrderPS, CurrentBillingPeriodEnd: &lockOrderPE, NextBillingAt: &lockOrderPE,
				Items: []domain.SubscriptionItem{{PlanID: planP.ID, Quantity: 1}},
			})
			return err
		}},
		{"add item", func() error {
			_, err := r.store.AddItem(at, r.tenantID, domain.SubscriptionItem{SubscriptionID: onQ["add"].ID, PlanID: planP.ID, Quantity: 1})
			return err
		}},
		{"immediate swap", func() error {
			_, err := r.store.ApplyItemPlanImmediately(at, r.tenantID, onQ["swap"].Items[0].ID, planP.ID, lockOrderPS.Add(time.Hour))
			return err
		}},
		{"scheduled swap", func() error {
			_, err := r.store.SetItemPendingPlan(at, r.tenantID, onQ["schedule"].Items[0].ID, planP.ID, lockOrderPE)
			return err
		}},
	} {
		t.Run(w.name, func(t *testing.T) {
			err := w.write()
			if !errors.Is(err, errs.ErrInvalidState) {
				t.Fatalf("write = %v, want an invalid-state refusal: plan P bills a meter the customer already has", err)
			}
		})
	}

	var live int
	if err := r.admin.QueryRowContext(r.ctx, `
		SELECT count(*) FROM subscription_items si JOIN subscriptions s ON s.id = si.subscription_id
		WHERE s.customer_id = $1 AND si.deleted_at IS NULL AND (si.plan_id = $2 OR si.pending_plan_id = $2)`,
		r.custID, planP.ID).Scan(&live); err != nil {
		t.Fatalf("count items on plan P: %v", err)
	}
	if live != 0 {
		t.Errorf("%d items of the customer on plan P, want 0", live)
	}
}
