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
// The total is every meter the invoice bills, each aggregated over the
// invoice's whole window (the period, or period start to the cancel), counted
// as capUsageTotals and billableUsageTotal count it. That includes the meters
// of an item removed or swapped mid-period: they bill on this invoice, so they
// count against its cap.
//
// Rounding: a scaled quantity is quantity x cap / total, rounded down to the
// 12 decimal places an invoice line stores (invoice_line_items.quantity_decimal
// is numeric(38,12)). Rounding down keeps the sum of the stored quantities at
// or under the cap; the remainder, under 10^-12 units a line, is not billed.
// Before #1192 the factor was divided out at 16 places, so three meters of 100
// against a cap of 200 billed 66.66666666666667 each: 200.00000000000001 in
// all, and more once the store rounded each line to 12 places.
//
// Granularity: the total is per meter over the whole window, and a meter
// billed over that one window bills exactly what the cap counted. A meter
// billed over two disjoint windows (an item removed and later added back) can
// bill more than its whole-window total: a max or last meter bills a peak per
// window, and a correction in one window does not reduce the other. The cap
// does not see that difference.

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

// usageCapFor returns sub's block usage cap for the invoice window [from, to).
// meterAggs is every meter the invoice bills and totals their aggregates over
// that window.
func (e *Engine) usageCapFor(ctx context.Context, sub domain.Subscription, meterAggs map[string]string, totals map[string]decimal.Decimal, from, to time.Time) (usageCap, error) {
	if sub.UsageCapUnits == nil || *sub.UsageCapUnits <= 0 || sub.OverageAction != "block" {
		return usageCap{}, nil
	}
	counted, err := e.capUsageTotals(ctx, sub, meterAggs, totals, from, to)
	if err != nil {
		return usageCap{}, fmt.Errorf("usage cap totals: %w", err)
	}
	return usageCap{limit: decimal.NewFromInt(*sub.UsageCapUnits), total: billableUsageTotal(counted)}, nil
}
