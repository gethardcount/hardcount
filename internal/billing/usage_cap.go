package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// A block usage cap (usage_cap_units with overage_action "block") is the most
// usage units one invoice bills across all its meters. When the usage passes
// the cap, every usage quantity on the invoice is scaled by the same factor,
// cap / total, so the meters keep their shares and the units add up to the
// cap. The period close and the cancel's final invoice both work the cap out
// here, once per invoice (#1192).
//
// The total counts each meter over the windows the invoice bills it for: the
// whole window for a meter on the subscription throughout, and only its own
// segment for a meter whose item was added, removed or swapped during the
// window. Usage outside those windows is not billed, so it does not count. A
// meter counts as the invoice bills it before the cap: a pricing-rule meter
// as the sum of its buckets above zero, any other meter at its total, and
// anything below zero as 0 (#1193). Unpriced usage counts too.
//
// The total and the usage lines come from the same reads (windowUsage), so a
// usage event that lands while the close runs cannot bill above the cap.
//
// Rounding: a scaled quantity is quantity x cap / total, rounded down to the
// 12 decimal places an invoice line stores (invoice_line_items.quantity_decimal
// is numeric(38,12)). Rounding down keeps the sum of the stored quantities at
// or under the cap; the remainder, under 10^-12 units a line, is not billed.
// Before #1192 the factor was divided out at 16 places, so three meters of 100
// against a cap of 200 billed 66.66666666666667 each: 200.00000000000001 in
// all, and more once the store rounded each line to 12 places.
//
// After a spend-threshold invoice (ADR-066) the windows are counted before the
// threshold clamp, so the total includes the usage that invoice billed while
// the close bills only what is left. Sum and count usage then bills from
// separate reads of the clamped windows, so the same-reads guarantee above
// does not cover it. The threshold invoice is not capped; that is the
// registered "usage caps with thresholds" gap.

// capQuantityPlaces is the scale of invoice_line_items.quantity_decimal.
const capQuantityPlaces = 12

// usageCap is one invoice's block usage cap. The zero value is no cap.
type usageCap struct {
	limit decimal.Decimal // usage_cap_units
	total decimal.Decimal // the usage the cap counts on this invoice
}

// binds reports whether the usage passes the cap, so the invoice's usage is
// scaled. A cap the usage only reaches bills every unit.
func (c usageCap) binds() bool {
	return c.total.GreaterThan(c.limit)
}

// apply returns qty as the invoice bills it. A quantity at or below zero is
// left alone: it bills 0, and the net-negative row reports its own value.
func (c usageCap) apply(qty decimal.Decimal) decimal.Decimal {
	if !c.binds() || !qty.IsPositive() {
		return qty
	}
	// QuoRem truncates the quotient at the given places: for a positive qty,
	// that is rounding down.
	scaled, _ := qty.Mul(c.limit).QuoRem(c.total, capQuantityPlaces)
	return scaled
}

// usageCapFor returns sub's block usage cap for one invoice. windows is each
// meter's billing windows on the invoice, before a threshold clamp; reads is
// the invoice's usage reader, which the usage lines then bill from.
func (e *Engine) usageCapFor(ctx context.Context, sub domain.Subscription, windows map[string][]usageInterval, reads *windowUsage) (usageCap, error) {
	if sub.UsageCapUnits == nil || *sub.UsageCapUnits <= 0 || sub.OverageAction != "block" {
		return usageCap{}, nil
	}
	total := decimal.Zero
	for meterID, ivs := range windows {
		rules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, sub.TenantID, meterID)
		if err != nil {
			return usageCap{}, fmt.Errorf("usage cap: list pricing rules for meter %s: %w", meterID, err)
		}
		for _, iv := range mergeUsageIntervals(ivs) {
			if len(rules) > 0 {
				buckets, err := reads.ruleBuckets(ctx, meterID, iv)
				if err != nil {
					return usageCap{}, fmt.Errorf("usage cap: %w", err)
				}
				for _, b := range buckets {
					if b.Quantity.IsPositive() {
						total = total.Add(b.Quantity)
					}
				}
				continue
			}
			q, err := reads.meterTotal(ctx, meterID, iv)
			if err != nil {
				return usageCap{}, fmt.Errorf("usage cap: %w", err)
			}
			if q.IsPositive() {
				total = total.Add(q)
			}
		}
	}
	return usageCap{limit: decimal.NewFromInt(*sub.UsageCapUnits), total: total}, nil
}

// windowUsage reads one invoice's usage, each window once. The usage cap
// counts from it and the usage lines bill from it, so both see the same
// numbers.
type windowUsage struct {
	e         *Engine
	sub       domain.Subscription
	meterAggs map[string]string                     // every meter the invoice bills → its aggregation
	totals    map[string]map[string]decimal.Decimal // window → meter → total
	buckets   map[string][]domain.RuleAggregation   // meter|window → pricing-rule buckets
}

func newWindowUsage(e *Engine, sub domain.Subscription, meterAggs map[string]string) *windowUsage {
	return &windowUsage{e: e, sub: sub, meterAggs: meterAggs,
		totals: map[string]map[string]decimal.Decimal{}, buckets: map[string][]domain.RuleAggregation{}}
}

func windowKey(iv usageInterval) string {
	return iv.start.UTC().Format(time.RFC3339Nano) + "|" + iv.end.UTC().Format(time.RFC3339Nano)
}

// meterTotal is the meter's usage over iv by its own aggregation. One read
// covers every meter of the invoice for that window.
func (w *windowUsage) meterTotal(ctx context.Context, meterID string, iv usageInterval) (decimal.Decimal, error) {
	k := windowKey(iv)
	t, ok := w.totals[k]
	if !ok {
		var err error
		t, err = w.e.usage.AggregateForBillingPeriodByAgg(ctx, w.sub.TenantID, w.sub.CustomerID, w.meterAggs, iv.start, iv.end)
		if err != nil {
			return decimal.Zero, fmt.Errorf("aggregate usage [%v, %v): %w", iv.start, iv.end, err)
		}
		w.totals[k] = t
	}
	return t[meterID], nil
}

// ruleBuckets is the meter's pricing-rule buckets over iv.
func (w *windowUsage) ruleBuckets(ctx context.Context, meterID string, iv usageInterval) ([]domain.RuleAggregation, error) {
	k := meterID + "|" + windowKey(iv)
	if b, ok := w.buckets[k]; ok {
		return b, nil
	}
	b, err := w.e.usage.AggregateByPricingRules(ctx, w.sub.TenantID, w.sub.CustomerID, meterID, mapMeterAggregation(w.meterAggs[meterID]), iv.start, iv.end)
	if err != nil {
		return nil, fmt.Errorf("aggregate by pricing rules for meter %s [%v, %v): %w", meterID, iv.start, iv.end, err)
	}
	w.buckets[k] = b
	return b, nil
}

// planMeterAggregations returns every meter on plans with its aggregation.
// Usage is customer+meter scoped, so a meter on two plans appears once. An
// unreadable meter is an error (retried per ADR-119): defaulting to "sum"
// billed a max or last meter as its total.
func (e *Engine) planMeterAggregations(ctx context.Context, tenantID string, plans map[string]domain.Plan) (map[string]string, error) {
	meterAggs := make(map[string]string)
	for _, pl := range plans {
		for _, meterID := range pl.MeterIDs {
			if _, ok := meterAggs[meterID]; ok {
				continue
			}
			m, err := e.pricing.GetMeter(ctx, tenantID, meterID)
			if err != nil {
				return nil, fmt.Errorf("get meter %s aggregation: %w", meterID, err)
			}
			meterAggs[meterID] = m.Aggregation
		}
	}
	return meterAggs, nil
}
