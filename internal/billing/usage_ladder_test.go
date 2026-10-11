package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

func graduated10then2() domain.RatingRuleVersion {
	return domain.RatingRuleVersion{ID: "rrv_g", Mode: domain.PricingGraduated, GraduatedTiers: []domain.RatingTier{
		{UpTo: 1000, UnitAmountCents: decimal.NewFromInt(10)},
		{UpTo: 0, UnitAmountCents: decimal.NewFromInt(2)},
	}}
}

func fireWatermark(lines ...domain.InvoiceLineItem) thresholdWatermark {
	t := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	return thresholdWatermark{billedThrough: &t, lines: lines}
}

func usageLine(meterID, rrv string, qty, amt int64) domain.InvoiceLineItem {
	return domain.InvoiceLineItem{LineType: domain.LineTypeUsage, MeterID: meterID, RatingRuleVersionID: rrv,
		Quantity: qty, QuantityDecimal: decimal.NewFromInt(qty), AmountCents: amt}
}

var d = decimal.NewFromInt

// P21: after a fire billed 700 units (7000¢), the close's 600 units continue
// the ladder: 300×10¢ + 300×2¢. Two later slices of the same bucket keep
// continuing; fire + close = F(total).
func TestUsageLadder_ContinuesFromTheFire(t *testing.T) {
	l := newUsageLadder(fireWatermark(usageLine("mtr", "rrv_g", 700, 7000)), false)
	rule := graduated10then2()
	a1, err := l.price(rule, "mtr", "mtr|", domain.AggSum, d(400))
	if err != nil || a1 != 3000+200 { // 300 at 10¢ to reach 1000, then 100 at 2¢
		t.Fatalf("slice 1 = %d (err %v), want 3200", a1, err)
	}
	a2, err := l.price(rule, "mtr", "mtr|", domain.AggSum, d(200))
	if err != nil || a2 != 400 {
		t.Fatalf("slice 2 = %d (err %v), want 400", a2, err)
	}
	if total, _ := domain.ComputeAmountCents(rule, d(1300)); 7000+a1+a2 != total {
		t.Errorf("fire + close = %d, want F(1300) = %d", 7000+a1+a2, total)
	}
}

// No fire, a non-additive bucket, or a bucket the fire did not bill: priced
// exactly as before (from tier 0).
func TestUsageLadder_UnchangedWithoutASeed(t *testing.T) {
	rule := graduated10then2()
	want, _ := domain.ComputeAmountCents(rule, d(600))
	if a, _ := newUsageLadder(thresholdWatermark{}, false).price(rule, "mtr", "mtr|", domain.AggSum, d(600)); a != want {
		t.Errorf("no fire: %d, want %d", a, want)
	}
	wm := fireWatermark(usageLine("mtr", "rrv_g", 700, 7000))
	if a, _ := newUsageLadder(wm, false).price(rule, "mtr", "mtr|", domain.AggMax, d(600)); a != want {
		t.Errorf("max bucket: %d, want %d (non-additive never continues)", a, want)
	}
	if a, _ := newUsageLadder(wm, false).price(rule, "other", "other|", domain.AggSum, d(600)); a != want {
		t.Errorf("unbilled meter: %d, want %d", a, want)
	}
}

// A package rule whose overage costs more than the package is non-monotone:
// a slice can floor at 0, and a later slice of the same bucket recovers it,
// so the period bills F(total) when it can.
func TestUsageLadder_NonMonotonePackageRecovers(t *testing.T) {
	rule := domain.RatingRuleVersion{ID: "rrv_p", Mode: domain.PricingPackage, PackageSize: 1000, PackageAmountCents: 500,
		OverageUnitAmountCents: decimal.NewFromInt(1)}
	fire, _ := domain.ComputeAmountCents(rule, d(999)) // 999¢
	l := newUsageLadder(fireWatermark(usageLine("mtr", "rrv_p", 999, fire)), false)
	a1, _ := l.price(rule, "mtr", "mtr|", domain.AggSum, d(1)) // F(1000)=500 < 999 billed
	a2, _ := l.price(rule, "mtr", "mtr|", domain.AggSum, d(999))
	total, _ := domain.ComputeAmountCents(rule, d(1999))
	if a1 != 0 || fire+a1+a2 != total {
		t.Errorf("slices %d, %d after fire %d: sum %d, want floor at 0 then F(1999) = %d", a1, a2, fire, fire+a1+a2, total)
	}
}

// Two fire lines for one (meter, version) cannot be told apart: loud.
func TestUsageLadder_AmbiguousFireLinesFail(t *testing.T) {
	l := newUsageLadder(fireWatermark(usageLine("mtr", "rrv_g", 300, 3000), usageLine("mtr", "rrv_g", 400, 4000)), false)
	if _, err := l.price(graduated10then2(), "mtr", "mtr|r1", domain.AggSum, d(10)); err == nil || !strings.Contains(err.Error(), "two usage lines") {
		t.Fatalf("err = %v, want the two-lines error", err)
	}
}

// Tax-inclusive mode persists the fire line's NET amount (ApplyTaxToLineItems
// rewrites AmountCents). The seed must be F(seed quantity), not that field,
// or the close bills the fire's tax again (review finding, 2026-10-11).
func TestUsageLadder_SeedIgnoresThePersistedNetAmount(t *testing.T) {
	net := usageLine("mtr", "rrv_g", 700, 5833) // 7000 gross at 20% inclusive VAT
	a, err := newUsageLadder(fireWatermark(net), false).price(graduated10then2(), "mtr", "mtr|", domain.AggSum, d(600))
	if err != nil || a != 3600 {
		t.Fatalf("close = %d (err %v), want 3600 = F(1300) − F(700); %d means the net seed leaked", a, err, 13000-5833-3400)
	}
}

// Flat rules and capped closes price exactly as before the ladder existed.
func TestUsageLadder_FlatAndCappedUnchanged(t *testing.T) {
	flat := domain.RatingRuleVersion{ID: "rrv_f", Mode: domain.PricingFlat, FlatAmountCents: decimal.NewFromInt(10)}
	wm := fireWatermark(usageLine("mtr", "rrv_f", 700, 7000), usageLine("mtr", "rrv_g", 700, 7000))
	if a, _ := newUsageLadder(wm, false).price(flat, "mtr", "mtr|", domain.AggSum, d(600)); a != 6000 {
		t.Errorf("flat after a fire = %d, want 6000 (unchanged)", a)
	}
	want, _ := domain.ComputeAmountCents(graduated10then2(), d(600))
	if a, _ := newUsageLadder(wm, true).price(graduated10then2(), "mtr", "mtr|", domain.AggSum, d(600)); a != want {
		t.Errorf("capped close = %d, want %d (ladder off under a usage cap)", a, want)
	}
}
