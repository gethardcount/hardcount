package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
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
