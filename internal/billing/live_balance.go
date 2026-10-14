package billing

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// CreditBalanceReader reads the credit a customer can spend right now: the
// ledger sum, less what is left of any grant that is past its expiry but has
// not been written off by the expiry sweep yet. A draw skips such a grant, so
// counting it would promise credit no invoice can use. Implemented by
// *credit.Service.
type CreditBalanceReader interface {
	SpendableBalance(ctx context.Context, tenantID, customerID string) (int64, error)
}

// CustomerSubscriptionLister lists a customer's subscriptions, hydrated with
// their items. Implemented by *subscription.PostgresStore.
type CustomerSubscriptionLister interface {
	List(ctx context.Context, filter subscription.ListFilter) ([]domain.Subscription, int, error)
}

// SetLiveBalanceReaders wires the two reads LiveCreditBalance composes.
// Required by the boot wiring check.
func (e *Engine) SetLiveBalanceReaders(credits CreditBalanceReader, subs CustomerSubscriptionLister) {
	e.creditBalances = credits
	e.customerSubs = subs
}

// LiveCreditBalance is the one place a customer's live credit figure is
// computed (issue #1019). The secret-key read and the public cost dashboard
// both call it, so the two can never disagree.
//
//	estimated remaining = max(spendable balance - unbilled usage, 0)
//
// Unbilled usage is what the period close would bill for usage if every
// billable subscription closed now: each subscription over its OWN current
// period, over the same per-meter windows, with the same threshold-watermark
// clamp and tier continuation the close applies (unbilledUsageCents). It
// lives in the engine because the figure is only right while it prices usage
// the way the close does; the earlier estimate in the usage package had
// drifted from the close in three ways (no watermark, trial usage counted,
// one window for every subscription).
//
// Left out on purpose, so the figure can read HIGH: tax, base fees billed in
// arrears (until the period closes), amounts due on unpaid invoices, and a
// grant that will expire before the period closes (it is spendable now, but
// will not be there for the close to draw on). It can read LOW for a
// subscription with a blocking usage cap, which the close applies and this
// does not. Currencies are added as raw cents; the ledger has no currency.
//
// The reads are separate transactions (balance, subscriptions, then per
// subscription the watermark and intervals, and per meter the aggregation).
// A threshold fire or period close that commits between them can make one
// response wrong: reading the balance before the invoice draws it and the
// usage after the period moves reads high. Nothing is stored here, so the
// next read is right again. A caller that stores the figure or acts on a
// crossing must close that gap first.
//
// Cost: one usage aggregation per meter window per subscription, each a scan
// of that customer's events in the window. Callers poll and cache; this is
// not a per-request check.
func (e *Engine) LiveCreditBalance(ctx context.Context, tenantID, customerID string) (domain.CreditLiveBalance, error) {
	if e.creditBalances == nil || e.customerSubs == nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: readers not wired")
	}
	balance, err := e.creditBalances.SpendableBalance(ctx, tenantID, customerID)
	if err != nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: read balance: %w", err)
	}
	subs, err := e.billableSubscriptions(ctx, tenantID, customerID)
	if err != nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: list subscriptions: %w", err)
	}
	var unbilled int64
	for _, sub := range subs {
		cents, err := e.unbilledUsageCents(ctx, sub)
		if err != nil {
			// No partial figure: leaving one subscription out would report
			// more credit than the customer has.
			return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: subscription %s: %w", sub.ID, err)
		}
		unbilled += cents
	}
	return domain.CreditLiveBalance{
		CustomerID:              customerID,
		BalanceCents:            balance,
		UnbilledUsageCents:      unbilled,
		EstimatedRemainingCents: max(balance-unbilled, 0),
		ComputedAt:              e.clock.Now(ctx),
	}, nil
}

// billableSubscriptions returns the customer's subscriptions whose stored
// current period the close will bill: every active one, and a trialing one
// unless its period closes without an invoice (trialPeriodBilled). It pages
// to the end because the list is capped per call, and a customer's canceled
// history must not push a live subscription off the page.
func (e *Engine) billableSubscriptions(ctx context.Context, tenantID, customerID string) ([]domain.Subscription, error) {
	const page = 100
	var out []domain.Subscription
	for _, status := range []domain.SubscriptionStatus{domain.SubscriptionActive, domain.SubscriptionTrialing} {
		for offset := 0; ; offset += page {
			subs, _, err := e.customerSubs.List(ctx, subscription.ListFilter{
				TenantID:   tenantID,
				CustomerID: customerID,
				Status:     string(status),
				Limit:      page,
				Offset:     offset,
			})
			if err != nil {
				return nil, err
			}
			for _, sub := range subs {
				if sub.Status == domain.SubscriptionTrialing && !trialPeriodBilled(sub) {
					continue
				}
				out = append(out, sub)
			}
			if len(subs) < page {
				break
			}
		}
	}
	return out, nil
}

// trialCancelsFree reports whether a trialing subscription has a cancel due
// at trial end. The engine then cancels it without an invoice (ADR-069).
func trialCancelsFree(sub domain.Subscription) bool {
	return sub.CancelAtPeriodEnd || (sub.CancelAt != nil && sub.TrialEndAt != nil && !sub.CancelAt.After(*sub.TrialEndAt))
}

// trialPeriodBilled reports whether the close will bill a trialing
// subscription's stored current period. The filter is the period, not the
// status. The subscription service stamps the first PAID period on a trialing
// row, starting at the beginning of the day the trial ends
// (firstPeriodAfterTrial); conversion opens the items' billing intervals at
// that period start, not at the flip, and the close bills usage from there.
// So usage sent on the trial's last day is billed, and it is counted here
// while the status still says trialing. Usage before the stored period start,
// the trial proper, falls outside the window on its own.
//
// Two trialing periods close without an invoice (handleTrialState): one the
// trial outlasts (the close only advances it), and one with a cancel due at
// trial end.
func trialPeriodBilled(sub domain.Subscription) bool {
	if trialCancelsFree(sub) {
		return false
	}
	if sub.TrialEndAt != nil && sub.CurrentBillingPeriodEnd != nil && sub.TrialEndAt.After(*sub.CurrentBillingPeriodEnd) {
		return false
	}
	return true
}

// unbilledUsageCents is the usage amount the period close would bill for
// this subscription's current period, given what a threshold invoice has
// already billed (ADR-066 §4). It follows buildLineItems:
//
//   - Each meter is counted over the windows a plan carrying it was on the
//     subscription (meterUsageIntervals, the close's own builder). After a
//     mid-period plan swap the outgoing plan's meter still counts up to the
//     swap and the incoming plan's meter counts only from it.
//   - No threshold invoice this period: every bucket over those windows.
//   - A reset=false threshold invoice exists: it billed usage up to its
//     billing_period_end, and credit already paid for it. Additive buckets
//     (sum, count) count only from that instant on, priced continuing the
//     tiers the invoice reached. Non-additive buckets (max, last) that the
//     invoice left off still count over the UNCLAMPED windows, because the
//     close bills them once, full-window. Clamping those would drop a peak
//     that predates the invoice.
//
// Usage is aggregated per customer and meter, with no subscription key, the
// same as the close. Two subscriptions on one meter would each bill the same
// events, which is why the subscription service refuses that setup
// (rejectMeterOverlap); if such rows exist the estimate counts the usage
// once per subscription, as the closes will.
//
// Not replicated from the close: the blocking usage cap.
func (e *Engine) unbilledUsageCents(ctx context.Context, sub domain.Subscription) (int64, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return 0, nil
	}
	periodStart, periodEnd := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd

	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return 0, err
	}

	plans := map[string]domain.Plan{}
	loadPlan := func(planID string) error {
		if _, ok := plans[planID]; ok || planID == "" {
			return nil
		}
		plan, err := e.pricing.GetPlan(ctx, sub.TenantID, planID)
		if err != nil {
			return fmt.Errorf("get plan %s: %w", planID, err)
		}
		plans[planID] = plan
		return nil
	}
	for _, it := range sub.Items {
		if err := loadPlan(it.PlanID); err != nil {
			return 0, err
		}
	}
	// A trialing subscription has no billing intervals yet: conversion opens
	// one per item at the stored period start. With no segments,
	// meterUsageIntervals gives each current item the whole period, which is
	// what those intervals will say.
	var segsByItem map[string][]baseSegment
	if sub.Status != domain.SubscriptionTrialing {
		segsByItem, err = e.windowSegments(ctx, sub, periodStart, periodEnd)
		if err != nil {
			return 0, fmt.Errorf("resolve segments: %w", err)
		}
		for _, segs := range segsByItem {
			for _, seg := range segs {
				if err := loadPlan(seg.planID); err != nil {
					return 0, err
				}
			}
		}
	}
	clamped, full := meterUsageIntervals(sub, segsByItem, plans, periodStart, periodEnd, wm.billedThrough)
	meterIDs := map[string]struct{}{}
	for mid := range clamped {
		meterIDs[mid] = struct{}{}
	}
	for mid := range full {
		meterIDs[mid] = struct{}{}
	}

	ladder := newUsageLadder(wm, false)
	var total int64
	for meterID := range meterIDs {
		price := func(rule domain.RatingRuleVersion, bucketKey string, mode domain.AggregationMode, qty decimal.Decimal) (int64, error) {
			return ladder.price(rule, meterID, bucketKey, mode, qty)
		}
		// sum adds the lines of one pass over the given windows: the clamped
		// pass takes every bucket except those the fire deferred, the full
		// pass only those. Rules are pinned at the period start (ADR-070).
		sum := func(windows []usageInterval, deferred bool) error {
			for _, iv := range mergeUsageIntervals(windows) {
				lines, _, _, err := e.rateMeterWindow(ctx, sub.TenantID, sub.CustomerID, meterID, iv.start, iv.end, periodStart, price)
				if err != nil {
					return fmt.Errorf("rate meter %s [%v, %v): %w", meterID, iv.start, iv.end, err)
				}
				for _, l := range lines {
					if wm.deferredBucket(l.AggregationMode, meterID, l.RatingRuleVersionID) == deferred {
						total += l.AmountCents
					}
				}
			}
			return nil
		}
		if err := sum(clamped[meterID], false); err != nil {
			return 0, err
		}

		// Full-window pass. Skipped for a meter that cannot have a deferred
		// bucket, so that after a fire a sum-only meter costs one aggregation
		// over the short post-fire window instead of a second one over the
		// whole period.
		if !wm.exists() {
			continue
		}
		nonAdditive, err := e.meterHasNonAdditiveBucket(ctx, sub.TenantID, meterID)
		if err != nil {
			return 0, err
		}
		if !nonAdditive {
			continue
		}
		if err := sum(full[meterID], true); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// meterHasNonAdditiveBucket reports whether any bucket of the meter (its
// default aggregation or a pricing rule's) is max / last, read from the
// meter's configuration rather than from which buckets have usage.
func (e *Engine) meterHasNonAdditiveBucket(ctx context.Context, tenantID, meterID string) (bool, error) {
	meter, err := e.pricing.GetMeter(ctx, tenantID, meterID)
	if err != nil {
		return false, fmt.Errorf("get meter %s: %w", meterID, err)
	}
	if nonAdditiveMode(mapMeterAggregation(meter.Aggregation)) {
		return true, nil
	}
	rules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
	if err != nil {
		return false, fmt.Errorf("list pricing rules for meter %s: %w", meterID, err)
	}
	for _, r := range rules {
		if nonAdditiveMode(r.AggregationMode) {
			return true, nil
		}
	}
	return false, nil
}
