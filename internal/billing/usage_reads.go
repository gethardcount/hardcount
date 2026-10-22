package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// windowUsage reads one invoice's usage, each window once: one read covers
// every meter of the invoice for a window, and a pricing-rule meter's
// buckets are read once per window. The cycle close and the cancel's final
// invoice bill their usage lines from it.
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
