package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Customer usage, the public cost dashboard and the margin report read a
// subscription's usage through the rater (ADR-130 §10), so the customer page
// prices usage the way the invoice does.

// RateCurrentPeriod rates the subscription's whole current period as its
// close would bill it if no threshold invoice had billed part of it: its
// plan windows, usage floors and tier ladder (rateCurrentPeriod). Usage a
// threshold invoice already billed is counted: this is the period's usage,
// not what is left to bill (the live balance's unbilled usage). A trialing
// subscription whose period closes without an invoice has no usage to bill.
func (e *Engine) RateCurrentPeriod(ctx context.Context, sub domain.Subscription) (domain.RatedSubscription, error) {
	if sub.Status == domain.SubscriptionTrialing && !trialPeriodBilled(sub) {
		return domain.RatedSubscription{}, nil
	}
	cp, err := e.rateCurrentPeriod(ctx, sub, wholePeriod)
	if err != nil {
		return domain.RatedSubscription{}, err
	}
	if sub.CurrentBillingPeriodStart == nil {
		return domain.RatedSubscription{}, nil
	}
	return domain.RatedSubscription{Usage: ratedRows(cp.buckets), MeterIDs: cp.meterIDs, Plans: cp.plans}, nil
}

// RateWindow rates the subscription's usage over [from, to): an estimate.
// Its plan windows (billing_intervals) are clipped to the window and each
// plan is priced at its pin of the current period, in the subscription's
// time domain (estimateSegments). Past periods are not priced at their own
// pins (ADR-130 §10). Nothing bills from this read. A subscription with no
// current period is priced as if its period started at `from`.
func (e *Engine) RateWindow(ctx context.Context, sub domain.Subscription, from, to time.Time) (domain.RatedSubscription, error) {
	plans, err := e.itemPlans(ctx, sub)
	if err != nil {
		return domain.RatedSubscription{}, err
	}
	segsByItem, pins, err := e.estimateSegments(ctx, sub, from, to)
	if err != nil {
		return domain.RatedSubscription{}, fmt.Errorf("resolve segments: %w", err)
	}
	if err := e.hydrateSegmentPlans(ctx, sub, segsByItem, plans); err != nil {
		return domain.RatedSubscription{}, err
	}
	src := e.prices()
	stretches := planStretches(sub, segsByItem, from, to)
	planMeters, err := planMetersAt(ctx, src, sub.TenantID, plans, pins)
	if err != nil {
		return domain.RatedSubscription{}, err
	}
	floors, err := e.loadUsageFloors(ctx, sub, from, to)
	if err != nil {
		return domain.RatedSubscription{}, err
	}
	clamped, _ := meterUsageIntervals(stretches, planMeters, nil, floors)
	var windows []rateWindow
	for _, k := range sortedPlanMeters(clamped) {
		for _, iv := range mergeUsageIntervals(clamped[k]) {
			windows = append(windows, rateWindow{planID: k.planID, meterID: k.meterID, iv: iv, pass: passEvery})
		}
	}
	r := rater{
		prices: src,
		reads:  claimReads{usage: e.usage, tenantID: sub.TenantID, customerID: sub.CustomerID},
		site: rateSite{preview: func(w rateWindow, err error) error {
			return fmt.Errorf("rate meter %s [%v, %v): %w", w.meterID, w.iv.start, w.iv.end, err)
		}},
		pins:       pins,
		tenantID:   sub.TenantID,
		customerID: sub.CustomerID,
	}
	buckets, err := r.rateUsageWindows(ctx, windows, nil)
	if err != nil {
		return domain.RatedSubscription{}, err
	}
	return domain.RatedSubscription{Usage: ratedRows(buckets), MeterIDs: meterSet(planMeters), Plans: plans}, nil
}

// ratedRows is each bucket as a RatedUsage row.
func ratedRows(buckets []ratedBucket) []domain.RatedUsage {
	out := make([]domain.RatedUsage, 0, len(buckets))
	for _, b := range buckets {
		row := domain.RatedUsage{
			PlanID:    b.w.planID,
			MeterID:   b.w.meterID,
			Quantity:  b.agg.Quantity,
			BelowZero: b.agg.Quantity.IsNegative(),
		}
		if b.priced {
			row.PriceID = b.price.ID
			row.DimensionMatch = lineMatch(b.price)
			row.AmountCents = b.amount
			row.UnitAmountDecimal = domain.DisplayUnitAmountDecimalFor(b.price.PriceShape, b.amount, b.agg.Quantity)
		}
		out = append(out, row)
	}
	return out
}
