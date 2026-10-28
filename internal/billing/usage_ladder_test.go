package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
)

func graduated10then2() domain.PlanPrice {
	return priceOf("prc_g", "mtr", domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{
		{UpTo: new(int64(1000)), UnitAmountCents: decimal.NewFromInt(10)},
		{UpTo: nil, UnitAmountCents: decimal.NewFromInt(2)},
	}})
}

// fireAt is the fire's end in these tests: the threshold watermark.
var fireAt = time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)

func fireWatermark(lines ...domain.InvoiceLineItem) thresholdWatermark {
	t := fireAt
	return thresholdWatermark{billedThrough: &t, lines: lines}
}

// usageLine is a fire's usage line for price over [period start, fire end).
func usageLine(meterID, priceID string, qty, amt int64) domain.InvoiceLineItem {
	start, end := fireAt.AddDate(0, 0, -9), fireAt
	return domain.InvoiceLineItem{LineType: domain.LineTypeUsage, MeterID: meterID, PriceID: priceID,
		Quantity: qty, QuantityDecimal: decimal.NewFromInt(qty), AmountCents: amt,
		BillingPeriodStart: &start, BillingPeriodEnd: &end}
}

// afterFire is the close's window of meterID that starts where the fire
// ended: the one window whose tiers continue.
func afterFire(meterID string) rateWindow {
	return rateWindow{meterID: meterID, iv: usageInterval{start: fireAt, end: fireAt.AddDate(0, 0, 20)}}
}

var d = decimal.NewFromInt

// P21: after a fire billed 700 units (7000¢), the close's 600 units continue
// the ladder: 300×10¢ + 300×2¢. Two later slices of the same bucket keep
// continuing; fire + close = F(total).
func TestUsageLadder_ContinuesFromTheFire(t *testing.T) {
	l := newUsageLadder(fireWatermark(usageLine("mtr", "prc_g", 700, 7000)))
	rule := graduated10then2()
	a1, err := l.price(rule, afterFire("mtr"), domain.AggregationSum, d(400))
	if err != nil || a1 != 3000+200 { // 300 at 10¢ to reach 1000, then 100 at 2¢
		t.Fatalf("slice 1 = %d (err %v), want 3200", a1, err)
	}
	a2, err := l.price(rule, afterFire("mtr"), domain.AggregationSum, d(200))
	if err != nil || a2 != 400 {
		t.Fatalf("slice 2 = %d (err %v), want 400", a2, err)
	}
	if total, _ := domain.ComputeAmountCents(rule.PriceShape, d(1300)); 7000+a1+a2 != total {
		t.Errorf("fire + close = %d, want F(1300) = %d", 7000+a1+a2, total)
	}
}

// No fire, a non-additive bucket, or a bucket the fire did not bill: priced
// exactly as before (from tier 0).
func TestUsageLadder_UnchangedWithoutASeed(t *testing.T) {
	rule := graduated10then2()
	want, _ := domain.ComputeAmountCents(rule.PriceShape, d(600))
	if a, _ := newUsageLadder(thresholdWatermark{}).price(rule, afterFire("mtr"), domain.AggregationSum, d(600)); a != want {
		t.Errorf("no fire: %d, want %d", a, want)
	}
	wm := fireWatermark(usageLine("mtr", "prc_g", 700, 7000))
	if a, _ := newUsageLadder(wm).price(rule, afterFire("mtr"), domain.AggregationMax, d(600)); a != want {
		t.Errorf("max bucket: %d, want %d (non-additive never continues)", a, want)
	}
	if a, _ := newUsageLadder(wm).price(rule, afterFire("other"), domain.AggregationSum, d(600)); a != want {
		t.Errorf("unbilled meter: %d, want %d", a, want)
	}
}

// A package continues across the fire too (ADR-130 §3): the fire billed a
// whole package for 999 units, so the close bills nothing more until the
// 1001st unit starts a second one. fire + close = F(total).
func TestUsageLadder_PackageContinuesFromTheFire(t *testing.T) {
	rule := priceOf("prc_p", "mtr", domain.PriceShape{Mode: domain.PricingPackage, PackageSize: 1000, PackageAmountCents: decimal.NewFromInt(500)})
	fire, _ := domain.ComputeAmountCents(rule.PriceShape, d(999)) // one whole package: 500¢
	l := newUsageLadder(fireWatermark(usageLine("mtr", "prc_p", 999, fire)))
	a1, err1 := l.price(rule, afterFire("mtr"), domain.AggregationSum, d(1)) // F(1000) = 500, already billed
	a2, err2 := l.price(rule, afterFire("mtr"), domain.AggregationSum, d(999))
	total, _ := domain.ComputeAmountCents(rule.PriceShape, d(1999))
	if err1 != nil || err2 != nil || fire != 500 || a1 != 0 || a2 != 500 || fire+a1+a2 != total {
		t.Errorf("fire %d, slices %d, %d (%v, %v): want 500, 0, 500 = F(1999) = %d", fire, a1, a2, err1, err2, total)
	}
}

// Two fire lines for one (meter, price) that end at the fire cannot be
// told apart: loud.
func TestUsageLadder_AmbiguousFireLinesFail(t *testing.T) {
	l := newUsageLadder(fireWatermark(usageLine("mtr", "prc_g", 300, 3000), usageLine("mtr", "prc_g", 400, 4000)))
	if _, err := l.price(graduated10then2(), afterFire("mtr"), domain.AggregationSum, d(10)); err == nil || !strings.Contains(err.Error(), "two usage lines") {
		t.Fatalf("err = %v, want the two-lines error", err)
	}
}

// Tax-inclusive mode persists the fire line's NET amount (ApplyTaxToLineItems
// rewrites AmountCents). The seed must be F(seed quantity), not that field,
// or the close bills the fire's tax again (review finding, 2026-10-11).
func TestUsageLadder_SeedIgnoresThePersistedNetAmount(t *testing.T) {
	net := usageLine("mtr", "prc_g", 700, 5833) // 7000 gross at 20% inclusive VAT
	a, err := newUsageLadder(fireWatermark(net)).price(graduated10then2(), afterFire("mtr"), domain.AggregationSum, d(600))
	if err != nil || a != 3600 {
		t.Fatalf("close = %d (err %v), want 3600 = F(1300) − F(700); %d means the net seed leaked", a, err, 13000-5833-3400)
	}
}

// Per-unit prices price exactly as before the ladder existed.
func TestUsageLadder_PerUnitUnchanged(t *testing.T) {
	flat := perUnit("prc_f", "mtr", 10)
	wm := fireWatermark(usageLine("mtr", "prc_f", 700, 7000), usageLine("mtr", "prc_g", 700, 7000))
	if a, _ := newUsageLadder(wm).price(flat, afterFire("mtr"), domain.AggregationSum, d(600)); a != 6000 {
		t.Errorf("per_unit after a fire = %d, want 6000 (unchanged)", a)
	}
}

// Tiers restart in each window (ADR-130 §9): only the close's window that
// starts where the fire ended continues the fire's ladder. A later window of
// the same price, after a swap back to its plan, prices from tier 0.
func TestUsageLadder_OnlyTheWindowAfterTheFireContinues(t *testing.T) {
	rule := graduated10then2()
	wm := fireWatermark(usageLine("mtr", "prc_g", 700, 7000))
	later := rateWindow{meterID: "mtr", iv: usageInterval{start: fireAt.AddDate(0, 0, 5), end: fireAt.AddDate(0, 0, 20)}}
	want, _ := domain.ComputeAmountCents(rule.PriceShape, d(600))
	if a, err := newUsageLadder(wm).price(rule, later, domain.AggregationSum, d(600)); err != nil || a != want {
		t.Errorf("a window that starts after the fire = %d (err %v), want %d from tier 0", a, err, want)
	}
	if a, _ := newUsageLadder(wm).price(rule, afterFire("mtr"), domain.AggregationSum, d(600)); a != 300*10+300*2 {
		t.Errorf("the window after the fire = %d, want 3600 (continued)", a)
	}
}
