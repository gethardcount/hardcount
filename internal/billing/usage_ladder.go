package billing

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
)

// usageLadder prices a closer's usage so a price's tiers continue from what a
// mid-period threshold fire already billed, instead of restarting at tier 0
// (P21, ADR-066 amendment 2026-10-11). Each slice bills F(quantity so far +
// q) − F-billed so far, so fire + close = F(total) for graduated and package
// prices. F never decreases as the quantity grows (a package bills
// ceil(q / size) packages, ADR-130 §3), so a slice is never negative; one
// that would be is an error, not a clamp.
//
// A ladder is keyed by price id (ADR-130 §9). Tiers restart in each window
// of a period, so only the window the fire split continues: the close's
// window that starts at the fire's end, and the fire's line for the same
// price that ends there. Every other window prices on its own.
//
// It prices exactly as before (ComputeAmountCents(price, qty)) when there is
// no fire, for a non-additive bucket, and for a per_unit price (linear: the
// seed changes nothing).
//
// The seed amount is F(seed quantity) recomputed under the price, never the
// fire line's persisted AmountCents: in tax-inclusive mode that field holds
// the NET amount (ApplyTaxToLineItems rewrites it), and subtracting it from a
// gross F would bill the fire's tax again.
type usageLadder struct {
	wm  thresholdWatermark
	on  bool
	qty map[string]decimal.Decimal // price id → quantity priced so far this period
	amt map[string]int64           // price id → amount billed so far this period
}

// newUsageLadder builds the closer's ladder. It is on whenever this period
// has a threshold fire to continue from.
func newUsageLadder(wm thresholdWatermark) *usageLadder {
	return &usageLadder{wm: wm, on: wm.exists(), qty: map[string]decimal.Decimal{}, amt: map[string]int64{}}
}

// continues reports whether a bucket of window w priced under price
// continues a ladder.
func (l *usageLadder) continues(price domain.PlanPrice, w rateWindow, aggregation string) bool {
	return l.on && !nonAdditive(aggregation) && price.Mode != domain.PricingPerUnit &&
		w.iv.start.Equal(*l.wm.billedThrough)
}

// price returns the amount for qty more units of window w under price, the
// RESOLVED price (override applied).
func (l *usageLadder) price(price domain.PlanPrice, w rateWindow, aggregation string, qty decimal.Decimal) (int64, error) {
	if !l.continues(price, w, aggregation) {
		return domain.ComputeAmountCents(price.PriceShape, qty)
	}
	if _, seen := l.qty[price.ID]; !seen {
		seed, err := l.wm.billedUsageQty(w.meterID, price.ID)
		if err != nil {
			return 0, err
		}
		seedAmt, err := domain.ComputeAmountCents(price.PriceShape, seed)
		if err != nil {
			return 0, err
		}
		l.qty[price.ID], l.amt[price.ID] = seed, seedAmt
	}
	cumulative := l.qty[price.ID].Add(qty)
	total, err := domain.ComputeAmountCents(price.PriceShape, cumulative)
	if err != nil {
		return 0, err
	}
	amount := total - l.amt[price.ID]
	if amount < 0 {
		return 0, fmt.Errorf("meter %s price %s: the price of %s units (%d) is below what this period already billed for fewer units (%d)", w.meterID, price.ID, cumulative, total, l.amt[price.ID])
	}
	l.qty[price.ID] = cumulative
	l.amt[price.ID] += amount
	return amount, nil
}

// billedUsageQty is the quantity the fire billed for one price over the
// window that ends at the fire's end — zero when the fire has no such line.
// A fire prices each (plan, meter) window once, so two such lines for one
// price mean an invoice the close cannot continue: an error.
func (w thresholdWatermark) billedUsageQty(meterID, priceID string) (decimal.Decimal, error) {
	var (
		qty   decimal.Decimal
		found bool
	)
	for _, li := range w.lines {
		if li.LineType != domain.LineTypeUsage || li.MeterID != meterID || li.PriceID != priceID {
			continue
		}
		if li.BillingPeriodEnd == nil || !li.BillingPeriodEnd.Equal(*w.billedThrough) {
			continue
		}
		if found {
			return decimal.Zero, fmt.Errorf("threshold invoice has two usage lines for meter %s price %s that end at %s, so the close cannot tell which one its tiers continue", meterID, priceID, w.billedThrough)
		}
		found = true
		qty = li.QuantityDecimal
		if qty.IsZero() {
			qty = decimal.NewFromInt(li.Quantity)
		}
	}
	return qty, nil
}
