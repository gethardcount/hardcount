package subscription

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestMeterOverlapGuard is the real-Postgres proof of the double-billing
// guard. Usage aggregation is customer+meter scoped with NO subscription
// filter (engine.AggregateForBillingPeriodByAgg), so two live subscriptions
// of one customer whose plans share a meter would each rate the same usage
// — the customer is invoiced twice for every token. Migration 0026 dropped
// the one-live-sub-per-customer index deferring enforcement to "application
// policy" that never existed until this guard.
//
// Only a real DB proves the conflict scan: it unnests plans.meter_ids JSONB
// across the customer's live items (including pending_plan_id), which the
// in-memory fake stubs out.
func TestMeterOverlapGuard(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Meter Overlap Guard")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_meter_guard", DisplayName: "Meter Guard",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	pricingStore := pricing.NewPostgresStore(db)
	newPlan := func(code string, meterIDs []string) domain.Plan {
		return createPlanPricing(t, db, ctx, tenantID, domain.Plan{
			Code: code, Name: code, Currency: "USD",
			BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears,
			BaseAmountCents: 1000, Status: domain.PlanActive,
		}, meterIDs...)
	}
	const meterTokens = "mtr_guard_tokens"
	const meterSeats = "mtr_guard_seats"
	planTokA := newPlan("guard-tok-a", []string{meterTokens})
	planTokB := newPlan("guard-tok-b", []string{meterTokens})
	planTokC := newPlan("guard-tok-c", []string{meterTokens})
	planSeats := newPlan("guard-seats", []string{meterSeats})
	planFlat := newPlan("guard-flat", nil)

	svc := NewService(NewPostgresStore(db), nil)
	svc.SetPlanReader(pricingStore)
	svc.SetPlanMeterReader(pricingStore)

	create := func(code string, planIDs ...string) (domain.Subscription, error) {
		items := make([]CreateItemInput, 0, len(planIDs))
		for _, pid := range planIDs {
			items = append(items, CreateItemInput{PlanID: pid, Quantity: 1})
		}
		return svc.Create(ctx, tenantID, CreateInput{
			Code: code, DisplayName: code, CustomerID: cust.ID,
			Items: items, BillingTime: domain.BillingTimeAnniversary,
		})
	}
	wantConflict := func(err error, wantSub string) {
		t.Helper()
		var de *errs.DomainError
		if !errors.As(err, &de) || de.Kind != errs.ErrInvalidState {
			t.Fatalf("want InvalidState conflict, got: %v", err)
		}
		if !strings.Contains(de.Message, meterTokens) {
			t.Fatalf("conflict message must name the meter: %q", de.Message)
		}
		if wantSub != "" && !strings.Contains(de.Message, wantSub) {
			t.Fatalf("conflict message must name sub %q: %q", wantSub, de.Message)
		}
	}

	// A live sub billing the tokens meter.
	subA, err := create("guard-sub-a", planTokA.ID)
	if err != nil {
		t.Fatalf("create subA: %v", err)
	}

	// Cross-sub create block: a second live sub on the same meter (via a
	// DIFFERENT plan) is the double-billing shape — every token would be
	// rated once per sub.
	if _, err := create("guard-sub-b", planTokB.ID); err == nil {
		t.Fatal("second live sub on the same meter must be rejected")
	} else {
		wantConflict(err, "guard-sub-a")
	}

	// Intra-create block: one request with two plans sharing a meter.
	if _, err := create("guard-sub-both", planTokB.ID, planTokC.ID); err == nil {
		t.Fatal("two items sharing a meter in one create must be rejected")
	} else {
		var de *errs.DomainError
		if !errors.As(err, &de) || de.Kind != errs.ErrValidation {
			t.Fatalf("want validation error for intra-create overlap, got: %v", err)
		}
	}

	// Disjoint meters stay allowed — the guard scopes to the money
	// invariant, not to one-sub-per-customer.
	subC, err := create("guard-sub-c", planSeats.ID)
	if err != nil {
		t.Fatalf("disjoint-meter sub must be allowed: %v", err)
	}

	// AddItem cross-sub block: adding a tokens-meter plan to subC would
	// double-bill against subA.
	if _, err := svc.AddItem(ctx, tenantID, subC.ID, AddItemInput{PlanID: planTokB.ID}); err == nil {
		t.Fatal("AddItem onto an already-billed meter must be rejected")
	} else {
		wantConflict(err, "guard-sub-a")
	}

	// Meterless plans pass the guard (base-fee-only add-on).
	if _, err := svc.AddItem(ctx, tenantID, subC.ID, AddItemInput{PlanID: planFlat.ID}); err != nil {
		t.Fatalf("meterless AddItem must be allowed: %v", err)
	}

	// Plan-swap block, scheduled variant: a pending swap onto the tokens
	// meter would start double-billing at the next cycle boundary.
	var seatsItemID string
	for _, it := range subC.Items {
		if it.PlanID == planSeats.ID {
			seatsItemID = it.ID
		}
	}
	if seatsItemID == "" {
		t.Fatal("subC seats item not found")
	}
	if _, err := svc.UpdateItem(ctx, tenantID, subC.ID, seatsItemID, UpdateItemInput{NewPlanID: planTokB.ID}); err == nil {
		t.Fatal("scheduled swap onto an already-billed meter must be rejected")
	} else {
		wantConflict(err, "guard-sub-a")
	}

	// Canceling frees the meter: after subA terminates, the meter is
	// billable by a new sub again.
	if _, _, err := svc.Cancel(ctx, tenantID, subA.ID); err != nil {
		t.Fatalf("cancel subA: %v", err)
	}
	subD, err := create("guard-sub-d", planTokB.ID)
	if err != nil {
		t.Fatalf("meter must be free after cancel: %v", err)
	}
	_ = subD

	// Pending plans count as billed: schedule subC's seats item onto a
	// fresh meter, then try to create a sub on that meter — the scheduled
	// swap already claims it for the next boundary.
	planSeatsV2 := newPlan("guard-seats-v2", []string{"mtr_guard_seats_v2"})
	planSeatsV2Dup := newPlan("guard-seats-v2-dup", []string{"mtr_guard_seats_v2"})
	if _, err := svc.UpdateItem(ctx, tenantID, subC.ID, seatsItemID, UpdateItemInput{NewPlanID: planSeatsV2.ID}); err != nil {
		t.Fatalf("scheduled swap onto a free meter must be allowed: %v", err)
	}
	if _, err := create("guard-sub-e", planSeatsV2Dup.ID); err == nil {
		t.Fatal("meter claimed by a pending swap must be rejected")
	} else {
		var de *errs.DomainError
		if !errors.As(err, &de) || de.Kind != errs.ErrInvalidState {
			t.Fatalf("want InvalidState for pending-plan conflict, got: %v", err)
		}
		if !strings.Contains(de.Message, "mtr_guard_seats_v2") {
			t.Fatalf("conflict message must name the pending meter: %q", de.Message)
		}
	}
}
