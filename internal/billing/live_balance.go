package billing

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// CreditBalanceReader reads a customer's settled credit balance: the ledger
// sum, which moves only when an invoice, grant, adjustment or expiry is
// written. Implemented by *credit.Service.
type CreditBalanceReader interface {
	GetBalance(ctx context.Context, tenantID, customerID string) (domain.CreditBalance, error)
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
//	estimated remaining = max(settled balance - unbilled usage, 0)
//
// Unbilled usage is what the period close would bill for usage if every
// active subscription closed now: each subscription over its OWN current
// period, with the same threshold-watermark clamp and tier continuation the
// close applies (unbilledUsageCents). It lives in the engine because the
// figure is only right while it prices usage the way the close does; the
// earlier estimate in the usage package had drifted from the close in three
// ways (no watermark, trials counted, one window for every subscription).
//
// Left out on purpose, so the figure can read HIGH: tax, base fees billed in
// arrears (until the period closes), and amounts due on unpaid invoices.
// It can read LOW for a subscription with a blocking usage cap, which the
// close applies and this does not. Currencies are added as raw cents; the
// ledger has no currency.
//
// The reads are separate transactions (balance, subscriptions, then per
// subscription the watermark and per meter the aggregation). A threshold
// fire or period close that commits between them can make one response
// wrong: reading the balance before the invoice draws it and the usage after
// the period moves reads high. Nothing is stored here, so the next read is
// right again. A caller that stores the figure or acts on a crossing must
// close that gap first.
//
// Cost: one usage aggregation per meter per active subscription, each a scan
// of that customer's events in the period. Callers poll and cache; this is
// not a per-request check.
func (e *Engine) LiveCreditBalance(ctx context.Context, tenantID, customerID string) (domain.CreditLiveBalance, error) {
	if e.creditBalances == nil || e.customerSubs == nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: readers not wired")
	}
	bal, err := e.creditBalances.GetBalance(ctx, tenantID, customerID)
	if err != nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: read balance: %w", err)
	}
	subs, err := e.activeSubscriptions(ctx, tenantID, customerID)
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
		BalanceCents:            bal.BalanceCents,
		UnbilledUsageCents:      unbilled,
		EstimatedRemainingCents: max(bal.BalanceCents-unbilled, 0),
		ComputedAt:              e.clock.Now(ctx),
	}, nil
}

// activeSubscriptions returns every ACTIVE subscription of the customer.
// Trialing subscriptions are left out: the close advances a trial period
// without an invoice (handleTrialState), so trial usage never draws credit.
// It pages to the end because the list is capped per call, and a customer's
// canceled history must not push an active subscription off the page.
func (e *Engine) activeSubscriptions(ctx context.Context, tenantID, customerID string) ([]domain.Subscription, error) {
	const page = 100
	var out []domain.Subscription
	for offset := 0; ; offset += page {
		subs, _, err := e.customerSubs.List(ctx, subscription.ListFilter{
			TenantID:   tenantID,
			CustomerID: customerID,
			Status:     string(domain.SubscriptionActive),
			Limit:      page,
			Offset:     offset,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, subs...)
		if len(subs) < page {
			return out, nil
		}
	}
}

// unbilledUsageCents is the usage amount the period close would bill for
// this subscription's current period, given what a threshold invoice has
// already billed (ADR-066 §4). It follows buildLineItems' two passes:
//
//   - No threshold invoice this period: every bucket over the whole period.
//   - A reset=false threshold invoice exists: it billed usage up to its
//     billing_period_end, and credit already paid for it. Additive buckets
//     (sum, count) count only from that instant on, priced continuing the
//     tiers the invoice reached. Non-additive buckets (max, last) that the
//     invoice left off still count over the WHOLE period, because the close
//     bills them once, full-window. Clamping those would drop a peak that
//     predates the invoice.
//
// Usage is aggregated per customer and meter, with no subscription key, the
// same as the close. Two subscriptions on one meter would each bill the same
// events, which is why the subscription service refuses that setup
// (rejectMeterOverlap); if such rows exist the estimate counts the usage
// once per subscription, as the closes will.
//
// Not replicated from the close: the blocking usage cap, and a meter that a
// mid-period plan change added or removed (the close bills it only for the
// time it was on the subscription).
func (e *Engine) unbilledUsageCents(ctx context.Context, sub domain.Subscription) (int64, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return 0, nil
	}
	periodStart, periodEnd := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd

	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return 0, err
	}
	clampedStart := periodStart
	if wm.exists() && clampedStart.Before(*wm.billedThrough) {
		clampedStart = *wm.billedThrough
	}
	ladder := newUsageLadder(wm, false)

	var total int64
	seen := map[string]struct{}{}
	for _, it := range sub.Items {
		plan, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return 0, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		for _, meterID := range plan.MeterIDs {
			if _, ok := seen[meterID]; ok {
				continue
			}
			seen[meterID] = struct{}{}

			// Clamped pass: everything except buckets the fire deferred.
			if periodEnd.After(clampedStart) {
				price := func(rule domain.RatingRuleVersion, bucketKey string, mode domain.AggregationMode, qty decimal.Decimal) (int64, error) {
					return ladder.price(rule, meterID, bucketKey, mode, qty)
				}
				lines, _, err := e.rateMeterWindow(ctx, sub.TenantID, sub.CustomerID, meterID, clampedStart, periodEnd, periodStart, price)
				if err != nil {
					return 0, fmt.Errorf("rate meter %s: %w", meterID, err)
				}
				for _, l := range lines {
					if !wm.deferredBucket(l.AggregationMode, meterID, l.RatingRuleVersionID) {
						total += l.AmountCents
					}
				}
			}

			// Full-window pass: only the buckets the fire deferred. Skipped
			// for a meter that cannot have one, so that after a fire a
			// sum-only meter costs one aggregation over the short post-fire
			// window instead of a second one over the whole period.
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
			lines, _, err := e.previewMeter(ctx, sub.TenantID, sub.CustomerID, meterID, periodStart, periodEnd)
			if err != nil {
				return 0, fmt.Errorf("rate meter %s over the full period: %w", meterID, err)
			}
			for _, l := range lines {
				if wm.deferredBucket(l.AggregationMode, meterID, l.RatingRuleVersionID) {
					total += l.AmountCents
				}
			}
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
