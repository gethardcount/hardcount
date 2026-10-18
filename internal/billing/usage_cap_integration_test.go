package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
)

// TestUsageCap_CycleClose_StoredUnitsStayWithinTheCap (#1192) on real
// Postgres: three meters of 100 units against a block usage cap of 200, one
// of them priced by a pricing rule. Each usage line stores 66.666666666666
// units (200/3 rounded down to the 12 places quantity_decimal keeps), so the
// stored lines add up to no more than the cap. Before the fix the tokens line
// billed all 100 units, and the other two stored 66.666666666667 each.
func TestUsageCap_CycleClose_StoredUnitsStayWithinTheCap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Usage Cap Scale")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	// calls (1c a call), tokens (pricing rule, 1c a model=a token) and seats (5c a seat).
	tokens, seats, planID := netNegativePlan(t, ctx, f, "cap3")
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_cap3", planID, 0)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET usage_cap_units = 200, overage_action = 'block' WHERE id = $1`, subID); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("set cap: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ingestUnpricedAt(t, ctx, f, custID, f.meterID, 100, f.cycleStart.Add(time.Minute), nil)
	ingestUnpricedAt(t, ctx, f, custID, tokens.ID, 100, f.cycleStart.Add(2*time.Minute), map[string]any{"model": "a"})
	ingestUnpricedAt(t, ctx, f, custID, seats.ID, 100, f.cycleStart.Add(3*time.Minute), nil)

	endPeriodNow(t, ctx, f, subID)
	if _, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50); len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	invs, _, err := f.invStore.List(ctx, invoice.ListFilter{TenantID: f.tenantID, SubscriptionID: subID})
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices = %d (err %v), want 1", len(invs), err)
	}
	lines, err := f.invStore.ListLineItems(ctx, f.tenantID, invs[0].ID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	want := decimal.RequireFromString("66.666666666666")
	sum, usageLines := decimal.Zero, 0
	for _, li := range lines {
		if li.LineType != domain.LineTypeUsage {
			continue
		}
		usageLines++
		if !li.QuantityDecimal.Equal(want) {
			t.Errorf("meter %s stored %s units, want %s", li.MeterID, li.QuantityDecimal, want)
		}
		sum = sum.Add(li.QuantityDecimal)
	}
	if usageLines != 3 {
		t.Errorf("usage lines = %d, want 3", usageLines)
	}
	if sum.GreaterThan(decimal.NewFromInt(200)) {
		t.Errorf("stored usage lines add up to %s units, over the cap of 200", sum)
	}
	// 66.666666666666 units: 67c of calls, 67c of tokens, 333c of seats.
	if invs[0].SubtotalCents != 467 {
		t.Errorf("subtotal = %d, want 467", invs[0].SubtotalCents)
	}
}

// TestUsageCap_CycleClose_RemovedItemCountsOnlyItsWindow (#1261 review) on
// real Postgres: the calls meter is on the subscription all period, and the
// seats meter is on an item removed an hour into it. Each has 100 billable
// units; seats has 100 more after the removal, which the invoice does not
// bill. Against a cap of 150 both bill 75 units. Counting the seats over the
// whole period (200 units) scaled both to 50.
func TestUsageCap_CycleClose_RemovedItemCountsOnlyItsWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, "Usage Cap Removed Item")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 120*time.Second)
	defer cancel()

	seats := seatsMeter(t, ctx, f, "seats_capwin")
	oldPlan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code: "pln_capwin_old", Name: "Removed", Currency: "USD",
		BillingInterval: domain.BillingMonthly, MeterIDs: []string{seats.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	custID, subID := seedUnpricedSub(t, ctx, f, "cus_capwin", f.planID, 0)
	removedAt := f.cycleStart.Add(time.Hour)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET usage_cap_units = 150, overage_action = 'block' WHERE id = $1`, subID); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("set cap: %v", err)
	}
	// The removed item's lifetime: its row is gone, its interval is closed.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, ends_at, source)
		VALUES ($1, $2, $3, $4, 1, $5, $6, 'create')
	`, f.tenantID, subID, postgres.NewID("vlx_si"), oldPlan.ID, f.cycleStart, removedAt); err != nil {
		postgres.Rollback(tx)
		t.Fatalf("insert removed item interval: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ingestUnpricedAt(t, ctx, f, custID, f.meterID, 100, f.cycleStart.Add(time.Minute), nil)
	ingestUnpricedAt(t, ctx, f, custID, seats.ID, 100, f.cycleStart.Add(2*time.Minute), nil)
	ingestUnpricedAt(t, ctx, f, custID, seats.ID, 100, removedAt.Add(time.Hour), nil) // after the removal: not billed

	endPeriodNow(t, ctx, f, subID)
	if _, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50); len(failures) > 0 {
		t.Fatalf("cycle failures: %v", failures)
	}
	invs, _, err := f.invStore.List(ctx, invoice.ListFilter{TenantID: f.tenantID, SubscriptionID: subID})
	if err != nil || len(invs) != 1 {
		t.Fatalf("invoices = %d (err %v), want 1", len(invs), err)
	}
	lines, err := f.invStore.ListLineItems(ctx, f.tenantID, invs[0].ID)
	if err != nil {
		t.Fatalf("list line items: %v", err)
	}
	want := decimal.NewFromInt(75)
	billed := map[string]decimal.Decimal{}
	for _, li := range lines {
		if li.LineType == domain.LineTypeUsage {
			billed[li.MeterID] = billed[li.MeterID].Add(li.QuantityDecimal)
		}
	}
	if len(billed) != 2 || !billed[f.meterID].Equal(want) || !billed[seats.ID].Equal(want) {
		t.Errorf("stored usage units = %v, want 75 of calls (%s) and 75 of seats (%s)", billed, f.meterID, seats.ID)
	}
	// 75 calls at 1c and 75 seats at 5c.
	if invs[0].SubtotalCents != 450 {
		t.Errorf("subtotal = %d, want 450", invs[0].SubtotalCents)
	}
}
