package billing

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// CreditBalanceReader reads the credit a customer can spend right now, per
// currency: the ledger sum, less what is left of any grant that is past its
// expiry but has not been written off by the expiry sweep yet. A draw skips
// such a grant, so counting it would promise credit no invoice can use.
// Implemented by *credit.Service.
type CreditBalanceReader interface {
	SpendableBalance(ctx context.Context, tenantID, customerID string) (map[string]int64, error)
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
// both call it, so the two can never disagree. The figure is per currency
// (ADR-129); in each one:
//
//	estimated remaining = max(spendable balance - unbilled usage, 0)
//
// A subscription's unbilled usage counts in the currency its close invoice
// will be issued in (Engine.invoiceCurrency, the function that stamps it), so
// it is set against the credit that invoice can draw. The result lists the
// sorted union of the ledger's currencies and the subscriptions'.
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
// will not be there for the close to draw on).
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
	balances, err := e.creditBalances.SpendableBalance(ctx, tenantID, customerID)
	if err != nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: read balance: %w", err)
	}
	subs, err := e.billableSubscriptions(ctx, tenantID, customerID)
	if err != nil {
		return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: list subscriptions: %w", err)
	}
	unbilled := map[string]int64{}
	for _, sub := range subs {
		cents, planCurrency, err := e.unbilledUsageCents(ctx, sub)
		if err != nil {
			// No partial figure: leaving one subscription out would report
			// more credit than the customer has.
			return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: subscription %s: %w", sub.ID, err)
		}
		currency, err := e.invoiceCurrency(ctx, sub, planCurrency)
		if err != nil {
			return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: subscription %s currency: %w", sub.ID, err)
		}
		cur := strings.ToUpper(currency)
		if unbilled[cur], err = addCents(unbilled[cur], cents); err != nil {
			return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: unbilled usage in %s: %w", cur, err)
		}
	}
	currencies := make([]string, 0, len(balances)+len(unbilled))
	for c := range balances {
		currencies = append(currencies, c)
	}
	for c := range unbilled {
		if _, ok := balances[c]; !ok {
			currencies = append(currencies, c)
		}
	}
	sort.Strings(currencies)
	out := domain.CreditLiveBalance{
		CustomerID: customerID,
		Balances:   make([]domain.CreditLiveCurrency, 0, len(currencies)),
		ComputedAt: e.clock.Now(ctx),
	}
	for _, c := range currencies {
		// unbilled is a sum of usage amounts, so never negative.
		remaining, err := addCents(balances[c], -unbilled[c])
		if err != nil {
			return domain.CreditLiveBalance{}, fmt.Errorf("live credit balance: remaining in %s: %w", c, err)
		}
		out.Balances = append(out.Balances, domain.CreditLiveCurrency{
			Currency:                c,
			BalanceCents:            balances[c],
			UnbilledUsageCents:      unbilled[c],
			EstimatedRemainingCents: max(remaining, 0),
		})
	}
	return out, nil
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
// already billed (ADR-066 §4): the priced buckets of rateCurrentPeriod.
//
// It also returns the currency of the subscription's first item's plan, the
// input Engine.invoiceCurrency takes to name the close invoice's currency.
func (e *Engine) unbilledUsageCents(ctx context.Context, sub domain.Subscription) (int64, string, error) {
	cp, err := e.rateCurrentPeriod(ctx, sub)
	if err != nil {
		return 0, "", err
	}
	var total int64
	for _, b := range cp.buckets {
		if b.priced {
			if total, err = addCents(total, b.amount); err != nil {
				return 0, "", fmt.Errorf("unbilled usage: %w", err)
			}
		}
	}
	return total, cp.planCurrency, nil
}

// currentPeriod is a subscription's current period rated as its close would
// bill it now.
type currentPeriod struct {
	buckets      []ratedBucket
	plans        map[string]domain.Plan
	planCurrency string
}

// rateCurrentPeriod rates the usage of this subscription's current period
// the way buildLineItems will:
//
//   - Each meter is counted over the windows a plan pricing it was on the
//     subscription (meterUsageIntervals, the close's own builder), each by
//     its own plan's prices at the period start. After a mid-period swap the
//     outgoing plan's window counts up to the swap at its prices and the
//     incoming plan's from it at its own. A window starts no earlier than
//     the trial end or another subscription's coverage end of the meter that
//     day (usageFloors).
//   - No threshold invoice this period: every bucket over those windows.
//   - A threshold invoice exists: it billed usage up to its
//     billing_period_end, and credit already paid for it. Additive buckets
//     (sum, count) count only from that instant on, priced continuing the
//     tiers the invoice reached. Non-additive buckets (max, last) that the
//     invoice left off still count over the UNCLAMPED windows, because the
//     close bills them once, full-window.
//
// Usage is aggregated per customer and meter, with no subscription key, the
// same as the close. Two live subscriptions on one meter would each bill the
// same events, which is why the subscription service refuses that setup
// (rejectMeterOverlap).
func (e *Engine) rateCurrentPeriod(ctx context.Context, sub domain.Subscription) (currentPeriod, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return currentPeriod{}, nil
	}
	periodStart, periodEnd := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd

	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return currentPeriod{}, err
	}
	plans, err := e.itemPlans(ctx, sub)
	if err != nil {
		return currentPeriod{}, err
	}
	// A trialing subscription has no billing intervals yet: conversion opens
	// one per item at the stored period start. With no segments,
	// meterUsageIntervals gives each current item the whole period, which is
	// what those intervals will say.
	var segsByItem map[string][]baseSegment
	if sub.Status != domain.SubscriptionTrialing {
		segsByItem, err = e.windowSegments(ctx, sub, periodStart, periodEnd)
		if err != nil {
			return currentPeriod{}, fmt.Errorf("resolve segments: %w", err)
		}
		if err := e.hydrateSegmentPlans(ctx, sub, segsByItem, plans); err != nil {
			return currentPeriod{}, err
		}
	}
	src := e.prices()
	pin := pinFor(sub, periodStart)
	planMeters, err := planMetersAt(ctx, src, sub.TenantID, plans, pin)
	if err != nil {
		return currentPeriod{}, err
	}
	meters, err := e.readMeters(ctx, sub.TenantID, planMeters)
	if err != nil {
		return currentPeriod{}, err
	}
	floors, err := e.loadUsageFloors(ctx, sub, periodStart, periodEnd)
	if err != nil {
		return currentPeriod{}, err
	}
	clamped, full := meterUsageIntervals(sub, segsByItem, planMeters, periodStart, periodEnd, wm.billedThrough, floors)

	// The closer's two passes: every bucket except a deferred one over the
	// clamped windows, then the deferred ones over the full windows. The full
	// pass is skipped for a meter that cannot have a deferred bucket, so that
	// after a fire a sum-only meter costs one aggregation over the short
	// post-fire window instead of a second one over the whole period.
	var windows []rateWindow
	for _, k := range sortedPlanMeters(clamped, full) {
		for _, iv := range mergeUsageIntervals(clamped[k]) {
			windows = append(windows, rateWindow{planID: k.planID, meterID: k.meterID, iv: iv, pass: passClamped})
		}
		if !wm.exists() || !nonAdditive(meters[k.meterID].Aggregation) {
			continue
		}
		for _, iv := range mergeUsageIntervals(full[k]) {
			windows = append(windows, rateWindow{planID: k.planID, meterID: k.meterID, iv: iv, pass: passFull})
		}
	}
	r := rater{
		prices: closeMeters{PriceSource: src, meters: meters},
		reads:  claimReads{usage: e.usage, tenantID: sub.TenantID, customerID: sub.CustomerID},
		site: rateSite{preview: func(w rateWindow, err error) error {
			return fmt.Errorf("rate meter %s [%v, %v): %w", w.meterID, w.iv.start, w.iv.end, err)
		}},
		tenantID:   sub.TenantID,
		customerID: sub.CustomerID,
	}
	buckets, err := r.rateUsageWindows(ctx, windows, pin, newUsageLadder(wm))
	if err != nil {
		return currentPeriod{}, err
	}
	out := currentPeriod{buckets: buckets, plans: plans}
	if len(sub.Items) > 0 {
		out.planCurrency = plans[sub.Items[0].PlanID].Currency
	}
	return out, nil
}

// itemPlans reads the plan of each of sub's items.
func (e *Engine) itemPlans(ctx context.Context, sub domain.Subscription) (map[string]domain.Plan, error) {
	plans := make(map[string]domain.Plan, len(sub.Items))
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok || it.PlanID == "" {
			continue
		}
		plan, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return nil, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = plan
	}
	return plans, nil
}
