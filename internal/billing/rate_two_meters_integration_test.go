package billing_test

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/pricing"
)

// Two graduated meters on one plan, each priced by its own rating rule (no
// pricing rules), across a threshold invoice. Each meter's tiers continue
// from what the threshold invoice billed on that meter: in the close-mirror
// preview, the live balance and the period close. A ladder shared between
// the meters bills one meter's usage on the other's tier position.
//
// Calls: 1,000 at 10¢, then 2¢. 700 before the fire ($70.00), 600 after:
// 300 at 10¢ + 300 at 2¢ = $36.00. Tokens: 500 at 4¢, then 1¢. 200 before
// the fire ($8.00), 600 after: 300 at 4¢ + 300 at 1¢ = $15.00.
func TestTwoGraduatedMeters_EachContinuesItsOwnTiersAcrossFire_RealStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	lf := newLiveFixture(t, "Two Graduated Meters", 100000)
	ctx := lf.ctx
	calls := lf.meterID
	tokens, err := lf.pricingSvc.CreateMeter(ctx, lf.tenantID, pricing.CreateMeterInput{
		EventName: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create tokens meter: %v", err)
	}
	plan, _, err := lf.pricingSvc.CreatePlan(ctx, lf.tenantID, pricing.CreatePlanInput{
		Code: "pln_two_grad", Name: "Two Graduated", Currency: "USD", BillingInterval: domain.BillingMonthly,
		Prices: []pricing.PriceInput{
			{MeterID: calls, PriceShape: gradRule("calls", []domain.RatingTier{
				{UpTo: new(int64(1000)), UnitAmountCents: decimal.NewFromInt(10)},
				{UpTo: nil, UnitAmountCents: decimal.NewFromInt(2)},
			}).shape},
			{MeterID: tokens.ID, PriceShape: gradRule("tokens", []domain.RatingTier{
				{UpTo: new(int64(500)), UnitAmountCents: decimal.NewFromInt(4)},
				{UpTo: nil, UnitAmountCents: decimal.NewFromInt(1)},
			}).shape},
		},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lf.exec(t, `UPDATE subscription_items SET plan_id = $1 WHERE subscription_id = $2`, plan.ID, lf.subID)
	lf.exec(t, `UPDATE billing_intervals SET plan_id = $1 WHERE subscription_id = $2`, plan.ID, lf.subID)

	lf.insertUsage(t, calls, 700, lf.cycleStart.Add(time.Minute))
	lf.insertUsage(t, tokens.ID, 200, lf.cycleStart.Add(2*time.Minute))
	fireEnd := lf.fire(t, 5000)
	invs := lf.listInvoices(t, ctx)
	if fire, _ := usageLineTotal(t, lf.thresholdFixture, ctx, invs[0].ID); fire != 7800 {
		t.Fatalf("threshold invoice usage = %d, want 7800 ($70.00 calls + $8.00 tokens)", fire)
	}
	lf.insertUsage(t, calls, 600, fireEnd.Add(100*time.Millisecond))
	lf.insertUsage(t, tokens.ID, 600, fireEnd.Add(200*time.Millisecond))

	want := map[string]int64{calls: 3600, tokens.ID: 1500}

	preview := lf.createPreview(t, ctx, lf.customerID, lf.subID)
	got := map[string]int64{}
	for _, l := range preview.Lines {
		if l.LineType == "usage" {
			got[l.MeterID] += l.AmountCents
		}
	}
	if got[calls] != want[calls] || got[tokens.ID] != want[tokens.ID] {
		t.Errorf("close-mirror preview usage = calls %d, tokens %d; want %d and %d", got[calls], got[tokens.ID], want[calls], want[tokens.ID])
	}

	if unbilled := lf.live(t).UnbilledUsageCents; unbilled != 5100 {
		t.Errorf("live balance unbilled usage = %d, want 5100 ($36.00 + $15.00)", unbilled)
	}

	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	got = map[string]int64{}
	for _, inv := range lf.listInvoices(t, ctx) {
		if inv.ID == invs[0].ID {
			continue
		}
		_, lines := usageLineTotal(t, lf.thresholdFixture, ctx, inv.ID)
		for _, l := range lines {
			got[l.MeterID] += l.AmountCents
		}
	}
	if got[calls] != want[calls] || got[tokens.ID] != want[tokens.ID] {
		t.Errorf("close usage = calls %d, tokens %d; want %d and %d", got[calls], got[tokens.ID], want[calls], want[tokens.ID])
	}
}
