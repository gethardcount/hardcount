package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
)

// windowUsage reads one invoice's usage, each window once: one read covers
// every meter of the invoice for a window, and a meter priced by several
// prices has its buckets read once per window and price set. The cycle close
// and the cancel's final invoice bill their usage lines from it.
//
// meters is every meter the invoice bills, read once for the close. The
// totals are read under these meters' aggregations, and the closers' rater
// reads its meters from here too (closeMeters), so the total, the routing
// and the ladder of a bucket agree even if an aggregation PATCH commits
// during the close (one is allowed until the meter's first event, ADR-130
// §2).
type windowUsage struct {
	e         *Engine
	sub       domain.Subscription
	meters    map[string]domain.Meter
	meterAggs map[string]string                     // meters → aggregation
	totals    map[string]map[string]decimal.Decimal // window → meter → total
	buckets   map[string][]domain.ClaimedUsage      // meter|window|claims → price buckets
}

func newWindowUsage(e *Engine, sub domain.Subscription, meters map[string]domain.Meter) *windowUsage {
	aggs := make(map[string]string, len(meters))
	for id, m := range meters {
		aggs[id] = m.Aggregation
	}
	return &windowUsage{e: e, sub: sub, meters: meters, meterAggs: aggs,
		totals: map[string]map[string]decimal.Decimal{}, buckets: map[string][]domain.ClaimedUsage{}}
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

// claimBuckets is the meter's buckets over iv, one per price, under the
// meter's aggregation.
func (w *windowUsage) claimBuckets(ctx context.Context, meterID string, meter domain.Meter, prices []domain.PlanPrice, iv usageInterval) ([]domain.ClaimedUsage, error) {
	claims := domain.ClaimsOf(prices)
	ids := make([]string, 0, len(claims))
	for _, c := range claims {
		ids = append(ids, c.PriceID)
	}
	k := meterID + "|" + windowKey(iv) + "|" + strings.Join(ids, ",")
	if b, ok := w.buckets[k]; ok {
		return b, nil
	}
	b, err := w.e.usage.AggregateClaims(ctx, w.sub.TenantID, w.sub.CustomerID, meterID, meter.Aggregation, claims, iv.start, iv.end)
	if err != nil {
		return nil, fmt.Errorf("aggregate by price for meter %s [%v, %v): %w", meterID, iv.start, iv.end, err)
	}
	w.buckets[k] = b
	return b, nil
}

// closeMeters is the closers' PriceSource: prices and rules as the engine
// reads them, and each meter as the close read it (windowUsage.meters).
type closeMeters struct {
	PriceSource
	meters map[string]domain.Meter
}

// Meter is the meter as the close read it. Every meter a closer rates is
// priced by one of the plans the close read its meters from
// (meterUsageIntervals), so a meter that is not there is a bug, not a read to
// make now.
func (c closeMeters) Meter(_ context.Context, _, meterID string) (domain.Meter, error) {
	m, ok := c.meters[meterID]
	if !ok {
		return domain.Meter{}, fmt.Errorf("meter %s is not on the plans the close read", meterID)
	}
	return m, nil
}
