package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// PreviewLine is one line of an invoice preview. Mirrors the line shape the
// cycle scan persists onto invoice_line_items, with two differences:
//
//   - Quantity is a decimal string on the wire (NUMERIC(38,12) precision
//     preserved end-to-end — fractional AI-usage primitives like GPU-hours
//     and cached-token ratios round-trip without precision loss).
//   - Multi-rule meters emit one line per (meter, rule) pair with
//     DimensionMatch echoed from the meter pricing rule (the canonical
//     pricing identity). Single-rule meters keep the one-line-per-meter
//     shape with no DimensionMatch.
//
// See docs/design-create-preview.md.
type PreviewLine struct {
	LineType            string          `json:"line_type"`
	Description         string          `json:"description"`
	MeterID             string          `json:"meter_id,omitempty"`
	MeterUnit           string          `json:"meter_unit,omitempty"`
	RatingRuleVersionID string          `json:"rating_rule_version_id,omitempty"`
	RuleKey             string          `json:"rule_key,omitempty"`
	DimensionMatch      map[string]any  `json:"dimension_match,omitempty"`
	Currency            string          `json:"currency"`
	Quantity            decimal.Decimal `json:"quantity"`
	UnitAmountCents     int64           `json:"unit_amount_cents"`
	AmountCents         int64           `json:"amount_cents"`
	PricingMode         string          `json:"pricing_mode,omitempty"`
	// AggregationMode is the rule BUCKET's aggregation (sum/count/max/
	// last_during_period/last_ever) — per pricing rule, not per meter, since
	// one meter can carry mixed modes. Internal-only (json:"-"): the
	// threshold scan classifies non-additive buckets with it (ADR-066 §4);
	// the public create_preview wire is unchanged.
	AggregationMode domain.AggregationMode `json:"-"`
	// PlanID + BaseBillTiming identify the base_fee line's plan, internal-
	// only (json:"-"): the threshold fire consumes them to skip prepaid
	// in_advance bases and prorate reset-mode bases PER LINE — replacing a
	// positional parallel-array contract that silently mis-attributed
	// plans if the emission order ever changed (2026-07-10 design review).
	PlanID         string            `json:"-"`
	BaseBillTiming domain.BillTiming `json:"-"`
	// NominalUnitAmountDecimal carries the flat-mode configured per-unit rate
	// (decimal cents) from the OVERRIDE-APPLIED rule, internal-only (json:"-")
	// so an overage invoice line built from this preview shows the clean
	// nominal rate like a cycle usage line (ADR-054 re-examination). Nil for
	// graduated/package/base_fee → the invoice line's effective-rate fallback.
	NominalUnitAmountDecimal *decimal.Decimal `json:"-"`
}

// PreviewTotal is one currency's roll-up across the preview's lines. We
// always emit a list (one entry per distinct currency) even when there's
// only one currency — consistent shape lets clients read totals[0]
// without branching on cardinality, and lines up with the customer-usage
// endpoint's totals shape so a single TS type covers both surfaces.
type PreviewTotal struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
}

// PreviewResult is the response shape for both the in-app
// GET /v1/billing/preview/{subscription_id} debug route and the public
// POST /v1/invoices/create_preview surface. PlanName is a joined list of
// the subscription's item plans (comma-separated) — the previous
// single-plan shape can't represent a multi-item sub.
type PreviewResult struct {
	CustomerID         string         `json:"customer_id"`
	SubscriptionID     string         `json:"subscription_id"`
	PlanName           string         `json:"plan_name"`
	BillingPeriodStart time.Time      `json:"billing_period_start"`
	BillingPeriodEnd   time.Time      `json:"billing_period_end"`
	Lines              []PreviewLine  `json:"lines"`
	Totals             []PreviewTotal `json:"totals"`
	Warnings           []string       `json:"warnings"`
	GeneratedAt        time.Time      `json:"generated_at"`

	// unpriced is each meter bucket that matched no price, for the
	// threshold fire (not part of the API response).
	unpriced []unpricedBucket
}

// Preview generates a dry-run invoice for a subscription without persisting
// anything. Walks every item, resolves its plan, and emits a base_fee line
// per item plus per-(meter, rule) usage lines via the priority+claim
// LATERAL JOIN — same code path the cycle scan uses, so preview math ==
// invoice math.
func (e *Engine) Preview(ctx context.Context, sub domain.Subscription) (PreviewResult, error) {
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return PreviewResult{}, fmt.Errorf("subscription has no billing period set")
	}
	if len(sub.Items) == 0 {
		return PreviewResult{}, fmt.Errorf("subscription has no items")
	}
	return e.previewCycle(ctx, sub)
}

// previewCycle estimates the invoice the cycle close will issue for the
// subscription's CURRENT period. It is what the operator surfaces show (the
// subscription page and create_preview's default window), so it mirrors the
// close rather than the threshold scan: base fees come from the close's own
// closeBaseFeeLines (a calendar stub, a mid-period add, a quantity change or
// a plan swap prorates exactly as the invoice will), and usage is rated over
// the same per-meter windows the close bills (meterUsageIntervals). Before
// 2026-10-16 the preview showed the full base fee for a 19-day calendar stub
// the close billed at 19/31 (M-SUB-1).
//
// A trialing subscription's current period is its first paid period (it
// starts at the trial end, firstPeriodAfterTrial), so its preview is the
// first invoice after the trial — see previewTrialEnd. The exception is a
// trial that outlasts the current period: the close at its end only advances
// it, so that keeps the flat window estimate.
func (e *Engine) previewCycle(ctx context.Context, sub domain.Subscription) (PreviewResult, error) {
	periodStart := *sub.CurrentBillingPeriodStart
	periodEnd := *sub.CurrentBillingPeriodEnd
	if sub.Status == domain.SubscriptionTrialing && sub.TrialEndAt != nil && sub.TrialEndAt.After(periodEnd) {
		return e.previewWithWindow(ctx, sub, periodStart, periodEnd)
	}
	return e.previewWindow(ctx, sub, periodStart, periodEnd, true)
}

// previewWithWindow is the explicit-period variant. Used by the
// create_preview service when the caller passes a custom window, and by the
// threshold scan, which persists these lines as a fire invoice and depends
// on the FULL (unprorated) in_arrears base line (it prorates reset-mode
// bases itself). Both paths converge in previewWindow so the per-line
// composition stays in one place.
func (e *Engine) previewWithWindow(ctx context.Context, sub domain.Subscription, periodStart, periodEnd time.Time) (PreviewResult, error) {
	return e.previewWindow(ctx, sub, periodStart, periodEnd, false)
}

// previewWindow composes the preview. asClose=true mirrors the cycle close
// for [periodStart, periodEnd] (previewCycle); asClose=false is the flat
// window estimate (previewWithWindow).
func (e *Engine) previewWindow(ctx context.Context, sub domain.Subscription, periodStart, periodEnd time.Time, asClose bool) (PreviewResult, error) {
	plans := make(map[string]domain.Plan, len(sub.Items))
	planNames := make([]string, 0, len(sub.Items))
	var allMeterIDs []string
	seenMeter := make(map[string]struct{})
	for _, it := range sub.Items {
		if _, ok := plans[it.PlanID]; ok {
			continue
		}
		pl, err := e.pricing.GetPlan(ctx, sub.TenantID, it.PlanID)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("get plan %s: %w", it.PlanID, err)
		}
		plans[it.PlanID] = pl
		planNames = append(planNames, pl.Name)
		for _, mid := range pl.MeterIDs {
			if _, ok := seenMeter[mid]; ok {
				continue
			}
			seenMeter[mid] = struct{}{}
			allMeterIDs = append(allMeterIDs, mid)
		}
	}

	// ADR-031: when ANY item is in_advance, the cycle invoice's
	// HEADER period shifts to the upcoming period (engine.billOnePeriod
	// does the same). Preview must mirror this so the operator's
	// "what does the next invoice look like" answer matches what
	// actually lands. Totals are unchanged either way — only the
	// period label differs.
	invoiceStart, invoiceEnd := periodStart, periodEnd
	for _, it := range sub.Items {
		if plans[it.PlanID].BaseBillTiming == domain.BillInAdvance {
			loc, err := e.tenantLocation(ctx, sub.TenantID)
			if err != nil {
				return PreviewResult{}, err
			}
			invoiceStart = periodEnd
			invoiceEnd = advanceBillingPeriod(periodEnd, plans[it.PlanID].BillingInterval, loc, sub.BillingAnchorDay)
			break
		}
	}

	result := PreviewResult{
		CustomerID:         sub.CustomerID,
		SubscriptionID:     sub.ID,
		PlanName:           strings.Join(planNames, ", "),
		BillingPeriodStart: invoiceStart,
		BillingPeriodEnd:   invoiceEnd,
		Lines:              []PreviewLine{},
		Warnings:           []string{},
		GeneratedAt:        e.clock.Now(ctx),
	}

	if asClose {
		return e.previewAsClose(ctx, sub, plans, allMeterIDs, periodStart, periodEnd, result)
	}

	// Base-fee lines from each subscription item's plan. Quantity here is
	// the item's seat / count (always integer); we still expose it as a
	// decimal for shape uniformity.
	//
	// in_advance items bill the FULL upcoming base (no proration —
	// preview at cycle close, partial-period creation already settled
	// by BillOnCreate). in_arrears items bill the period being previewed.
	for _, it := range sub.Items {
		plan := plans[it.PlanID]
		if plan.BaseAmountCents <= 0 {
			continue
		}
		baseFee := plan.BaseAmountCents * it.Quantity
		desc := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, it.Quantity)
		if plan.BaseBillTiming == domain.BillInAdvance {
			desc = fmt.Sprintf("%s - base fee (qty %d, in advance for upcoming period)", plan.Name, it.Quantity)
		}
		result.Lines = append(result.Lines, PreviewLine{
			LineType:        "base_fee",
			Description:     desc,
			Currency:        plan.Currency,
			Quantity:        decimal.NewFromInt(it.Quantity),
			UnitAmountCents: plan.BaseAmountCents,
			AmountCents:     baseFee,
			PlanID:          plan.ID,
			BaseBillTiming:  plan.BaseBillTiming,
		})
	}

	// Usage lines per (meter, rule). The priority+claim LATERAL JOIN inside
	// AggregateByPricingRules is the canonical pricing identity — the
	// cycle scan calls the same path, so a multi-dim tenant's preview
	// matches what its actual invoice will be.
	//
	// A previewMeter ERROR aborts the whole preview (ADR-070,
	// no-silent-fallbacks). Only a meter with no rule binding at all and a
	// net-negative bucket (usage corrections) are soft cases, surfaced as
	// warnings from INSIDE previewMeter. A missing pinned rule, a rating
	// failure and store or resolution errors all reach this err, and a
	// partial preview built from those is a lie — worse, the threshold
	// scan PERSISTS these lines as a fire invoice, and a silently
	// dropped meter's pre-fire usage would then be billed by nobody:
	// the cycle close clamps every meter's window to the fire
	// watermark, deferral only exists for non-additive buckets. The
	// pre-ADR-070 warning-degradation here defeated previewMeter's own
	// fail-loud contract for exactly the money path it matters on.
	usageLineCount := 0
	for _, meterID := range allMeterIDs {
		meterLines, warnings, unpriced, err := e.previewMeter(ctx, sub.TenantID, sub.CustomerID, meterID, periodStart, periodEnd)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview meter %q: %w", meterID, err)
		}
		result.Lines = append(result.Lines, meterLines...)
		result.Warnings = append(result.Warnings, warnings...)
		result.unpriced = append(result.unpriced, unpriced...)
		usageLineCount += len(meterLines)
	}

	// Estimate-scope warnings (ADR-045): create_preview is a full-period
	// estimate that does NOT replicate the two overlays the cycle scan
	// applies — usage-cap scaling and mid-period segment proration. Rather
	// than silently overstating a capped sub or misstating one that changed
	// mid-period, surface the divergence on the existing warnings channel so
	// the estimate is honest about its own scope. (Replicating the overlays
	// is the deferred ADR-045 follow-up; this is honesty now, precision when
	// a design partner turns the features on.)
	//
	// Cap: only when there's billable usage the cap could scale — a
	// configured cap on a zero-usage period can't bind, so no warning.
	if msg, ok := capWarning(sub, usageLineCount); ok {
		result.Warnings = append(result.Warnings, msg)
	}

	// Segment proration: a mid-LIFE change within [periodStart, periodEnd]
	// makes the cycle bill base fees and usage per segment (each at its own
	// rate × duration), which the full-period estimate flattens. Only plan
	// swaps, quantity changes, and item removals qualify — the 'add' rows
	// include every item's INITIAL creation (emitted by the subscription_items
	// insert trigger, migration 0029), which is not a mid-period change and
	// would false-positive the warning on every sub in its first period. Since
	// the ADR-012 amendment (2026-07-26) the interval writers agree:
	// a period-start-day 'add' opens its interval at periodStart (write-time
	// day-grade, ADR-101), so a first period bills the full base and this filter matches the money. Same
	// [periodStart, periodEnd] window the cycle's segment-aware billing keys
	// off. e.subs is always wired in production and the create_preview path;
	// the nil-guard only spares narrow subs-less unit tests (the warning is
	// advisory, never a number).
	if e.subs != nil {
		itemChanges, err := e.subs.ListItemChangesInPeriod(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview: list item changes: %w", err)
		}
		midLifeChange := false
		for _, c := range itemChanges {
			if c.ChangeType != "add" { // plan | quantity | remove
				midLifeChange = true
				break
			}
		}
		if midLifeChange {
			result.Warnings = append(result.Warnings,
				"Estimate excludes mid-period proration; the plan, quantity, or item set changed during this period, so the actual invoice may rate base fees and usage per segment.")
		}
	}

	result.Totals = computePreviewTotals(result.Lines)
	return result, nil
}

// capWarning is the estimate-scope note for a block-mode usage cap, which
// neither preview replicates (ADR-045). Only when there is billable usage
// the cap could scale — a cap on a zero-usage period can't bind.
func capWarning(sub domain.Subscription, usageLineCount int) (string, bool) {
	if usageLineCount > 0 && sub.UsageCapUnits != nil && *sub.UsageCapUnits > 0 && sub.OverageAction == "block" {
		return fmt.Sprintf(
			"Estimate excludes the subscription's usage cap (%d units per period); if billable usage exceeds the cap, the actual invoice will be lower.",
			*sub.UsageCapUnits), true
	}
	return "", false
}

// previewAsClose is previewCycle's composition: the lines the cycle close
// would build for [periodStart, periodEnd] from the usage recorded so far.
// It reads the same inputs the close reads — billing_intervals segments,
// the threshold watermark, the invoice currency, the tenant zone — and
// shares the base-fee math (closeBaseFeeLines) and the usage windows
// (meterUsageIntervals) with buildLineItems. What it does not replicate is
// named in a warning: the usage cap (capWarning), and the tier ladder and
// deferred max/last buckets after a threshold fire (watermark warning).
func (e *Engine) previewAsClose(ctx context.Context, sub domain.Subscription, plans map[string]domain.Plan, meterOrder []string, periodStart, periodEnd time.Time, result PreviewResult) (PreviewResult, error) {
	loc, err := e.tenantLocation(ctx, sub.TenantID)
	if err != nil {
		return PreviewResult{}, err
	}
	currency, err := e.invoiceCurrency(ctx, sub, plans[sub.Items[0].PlanID].Currency)
	if err != nil {
		return PreviewResult{}, err
	}
	if sub.Status == domain.SubscriptionTrialing && trialCancelsFree(sub) {
		// handleTrialState cancels free at trial end: no invoice.
		result.Warnings = append(result.Warnings, "A cancel is scheduled for the end of the trial, so no invoice will be issued.")
		result.Totals = computePreviewTotals(result.Lines)
		return result, nil
	}
	if sub.Status == domain.SubscriptionTrialing {
		if res, ok, err := e.previewTrialEnd(ctx, sub, plans, meterOrder, periodStart, periodEnd, loc, result); ok || err != nil {
			return res, err
		}
	}
	wm, err := e.loadThresholdWatermark(ctx, sub.TenantID, sub.ID, periodStart, periodEnd)
	if err != nil {
		return PreviewResult{}, err
	}
	var segsByItem map[string][]baseSegment
	if sub.Status == domain.SubscriptionTrialing {
		segsByItem = trialConversionSegments(sub, periodStart, periodEnd)
	} else {
		segsByItem, err = e.windowSegments(ctx, sub, periodStart, periodEnd)
		if err != nil {
			return PreviewResult{}, fmt.Errorf("preview: resolve segments: %w", err)
		}
	}
	// Plans referenced only by a segment (a removed item, a pre-swap plan)
	// bill their segment too — same hydration as buildLineItems.
	for _, segs := range segsByItem {
		for _, seg := range segs {
			if seg.planID == "" {
				continue
			}
			if _, ok := plans[seg.planID]; ok {
				continue
			}
			pl, err := e.pricing.GetPlan(ctx, sub.TenantID, seg.planID)
			if err != nil {
				return PreviewResult{}, fmt.Errorf("get segment plan %s: %w", seg.planID, err)
			}
			plans[seg.planID] = pl
		}
	}

	// The close runs at periodEnd; a scheduled cancel firing there skips
	// the upcoming in_advance base.
	terminal := shouldFireScheduledCancel(sub, periodEnd, periodEnd)
	for _, bl := range closeBaseFeeLines(sub, segsByItem, plans, periodStart, periodEnd, currency, loc, terminal, wm.billedThrough != nil) {
		result.Lines = append(result.Lines, PreviewLine{
			LineType:        "base_fee",
			Description:     bl.line.Description,
			Currency:        bl.line.Currency,
			Quantity:        decimal.NewFromInt(bl.line.Quantity),
			UnitAmountCents: bl.line.UnitAmountCents,
			AmountCents:     bl.line.AmountCents,
			PlanID:          bl.plan.ID,
			BaseBillTiming:  bl.plan.BaseBillTiming,
		})
	}

	// Usage: each meter over the windows a plan carrying it was on the
	// subscription, clamped to the threshold watermark — the close's
	// meterUsageIntervals. Meters only a segment plan carries come after the
	// current plans' meters.
	order := append([]string(nil), meterOrder...)
	seen := make(map[string]struct{}, len(order))
	for _, mid := range order {
		seen[mid] = struct{}{}
	}
	for _, pl := range plans {
		for _, mid := range pl.MeterIDs {
			if _, ok := seen[mid]; !ok {
				seen[mid] = struct{}{}
				order = append(order, mid)
			}
		}
	}
	meterIntervals, _ := meterUsageIntervals(sub, segsByItem, plans, periodStart, periodEnd, wm.billedThrough)
	capped := sub.UsageCapUnits != nil && *sub.UsageCapUnits > 0 && sub.OverageAction == "block"
	ladder := newUsageLadder(wm, capped)
	usageLineCount := 0
	for _, meterID := range order {
		price := func(rule domain.RatingRuleVersion, bucketKey string, mode domain.AggregationMode, qty decimal.Decimal) (int64, error) {
			return ladder.price(rule, meterID, bucketKey, mode, qty)
		}
		for _, iv := range mergeUsageIntervals(meterIntervals[meterID]) {
			lines, warnings, unpriced, err := e.rateMeterWindow(ctx, sub.TenantID, sub.CustomerID, meterID, iv.start, iv.end, periodStart, price)
			if err != nil {
				return PreviewResult{}, fmt.Errorf("preview meter %q: %w", meterID, err)
			}
			for i := range lines {
				lines[i].Currency = currency
			}
			result.Lines = append(result.Lines, lines...)
			result.Warnings = append(result.Warnings, warnings...)
			result.unpriced = append(result.unpriced, unpriced...)
			usageLineCount += len(lines)
		}
	}

	if msg, ok := capWarning(sub, usageLineCount); ok {
		result.Warnings = append(result.Warnings, msg)
	}
	if wm.billedThrough != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"A spend-threshold invoice already billed this period through %s; this estimate shows only what is left. Tiered prices and max or last-value meters may differ on the actual invoice.",
			wm.billedThrough.In(loc).Format("Jan 2, 2006 15:04 MST")))
	}

	result.Totals = computePreviewTotals(result.Lines)
	return result, nil
}

// trialConversionSegments are the segments a trialing subscription's items
// will have once the trial converts. A trialing sub has no billing_intervals
// rows yet: the conversion opens every item at the post-trial period start
// (intervalOpenForItems, source "trial_convert"), which is the current
// period's start. Derived through intervalSegmentsByItem, the reader the
// close uses, so the preview clips them exactly as the close will.
func trialConversionSegments(sub domain.Subscription, periodStart, periodEnd time.Time) map[string][]baseSegment {
	rows := make(map[string][]domain.ItemInterval, len(sub.Items))
	for _, it := range sub.Items {
		rows[it.ID] = []domain.ItemInterval{{
			SubscriptionItemID: it.ID, PlanID: it.PlanID, Quantity: it.Quantity, StartsAt: periodStart,
		}}
	}
	return intervalSegmentsByItem(rows, periodStart, periodEnd)
}

// previewTrialEnd is the preview of a trialing subscription with an
// in_advance item: the first invoice after the trial is the one the trial
// end issues (ActivateAfterTrialWithBill → BillOnCreateTx), the in_advance
// base fees for the first paid period, prorated on a calendar stub. Its
// lines come from onCreateBaseLines, the math that invoice uses. ok=false
// when no item is in_advance: the first invoice is then the first period's
// close, which previewAsClose composes from trialConversionSegments.
func (e *Engine) previewTrialEnd(ctx context.Context, sub domain.Subscription, plans map[string]domain.Plan, meterIDs []string, periodStart, periodEnd time.Time, loc *time.Location, result PreviewResult) (PreviewResult, bool, error) {
	// Every item shares one bill timing (rejectMixedItemIntervals on every
	// item write; a live plan's timing is immutable), so either all items
	// are in_advance or none is.
	var advanceItems []domain.SubscriptionItem
	for _, it := range sub.Items {
		if plans[it.PlanID].BaseBillTiming == domain.BillInAdvance {
			advanceItems = append(advanceItems, it)
		}
	}
	if len(advanceItems) == 0 {
		return PreviewResult{}, false, nil
	}
	currency, err := e.invoiceCurrency(ctx, sub, plans[advanceItems[0].PlanID].Currency)
	if err != nil {
		return PreviewResult{}, true, err
	}
	for _, bl := range onCreateBaseLines(sub, plans, advanceItems, periodStart, periodEnd, currency, loc) {
		result.Lines = append(result.Lines, PreviewLine{
			LineType:        "base_fee",
			Description:     bl.line.Description,
			Currency:        bl.line.Currency,
			Quantity:        decimal.NewFromInt(bl.line.Quantity),
			UnitAmountCents: bl.line.UnitAmountCents,
			AmountCents:     bl.line.AmountCents,
			PlanID:          bl.plan.ID,
			BaseBillTiming:  bl.plan.BaseBillTiming,
		})
	}
	// The trial-end invoice covers the first paid period itself, not the
	// one after it (the in_advance header shift does not apply).
	result.BillingPeriodStart, result.BillingPeriodEnd = periodStart, periodEnd
	if len(meterIDs) > 0 {
		result.Warnings = append(result.Warnings,
			"This is the invoice issued when the trial ends. Usage for this period is on the invoice at the end of the period.")
	}
	result.Totals = computePreviewTotals(result.Lines)
	return result, true, nil
}

// previewMeter rates one meter's events for the customer over [from, to)
// and emits one PreviewLine per rule bucket. Mirrors usage.rateMeter from
// internal/usage/customer_usage.go — the cycle scan calls the same
// AggregateByPricingRules + ComputeAmountCents path, so each line
// produced here matches the invoice_line_item the cycle scan would
// persist.
func (e *Engine) previewMeter(ctx context.Context, tenantID, customerID, meterID string, from, to time.Time) ([]PreviewLine, []string, []unpricedBucket, error) {
	return e.rateMeterWindow(ctx, tenantID, customerID, meterID, from, to, from, nil)
}

// bucketPricer prices qty units of one usage bucket under its resolved rule.
// bucketKey is the closer's bucket identity (meter + pricing rule), the key
// usageLadder.price continues tiers under.
type bucketPricer func(rule domain.RatingRuleVersion, bucketKey string, mode domain.AggregationMode, qty decimal.Decimal) (int64, error)

// rateMeterWindow is previewMeter with the two things a window that does not
// start at the period open needs (the live credit read after a threshold
// fire, live_balance.go): pinAt, the instant rules and overrides are resolved
// at (always the period start, ADR-070, even when the window opens later at
// the fire's watermark), and price, which lets the caller continue a tier
// ladder across the fire the way the period close does. A nil price rates the
// bucket on its own, which is the preview and threshold-fire behavior.
// The unpriced buckets come back both as a warning (for people) and as data
// (for the threshold fire, which reports them).
func (e *Engine) rateMeterWindow(ctx context.Context, tenantID, customerID, meterID string, from, to, pinAt time.Time, price bucketPricer) ([]PreviewLine, []string, []unpricedBucket, error) {
	meter, err := e.pricing.GetMeter(ctx, tenantID, meterID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get meter: %w", err)
	}

	defaultMode := mapMeterAggregation(meter.Aggregation)
	aggs, err := e.usage.AggregateByPricingRules(ctx, tenantID, customerID, meterID, defaultMode, from, to)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("aggregate by pricing rules: %w", err)
	}

	rules, err := e.pricing.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list pricing rules: %w", err)
	}
	rulesByID := make(map[string]domain.MeterPricingRule, len(rules))
	for _, rule := range rules {
		rulesByID[rule.ID] = rule
	}

	var lines []PreviewLine
	var warnings []string
	var unpriced []unpricedBucket

	for _, agg := range aggs {
		if agg.Quantity.IsZero() {
			continue
		}
		// Ingest accepts negative corrections, so a bucket's period-to-date
		// total can be negative until new usage overtakes it. That is a
		// legitimate state, not a pricing error: it contributes nothing to
		// a preview or a threshold, with a warning. (Pricing it would fail —
		// ComputeAmountCents refuses negative quantities — and abort the
		// whole threshold evaluation for the subscription.)
		if agg.Quantity.IsNegative() {
			warnings = append(warnings, fmt.Sprintf("meter %q: net-negative usage so far (%s) after corrections — counted as 0", meter.Key, agg.Quantity.String()))
			continue
		}

		ratingRuleID := agg.RatingRuleVersionID
		if ratingRuleID == "" {
			ratingRuleID = meter.RatingRuleVersionID
		}
		if ratingRuleID == "" {
			// The quantity is what the operator needs to bill it by hand
			// (a one-off invoice), so the warning carries it (#981).
			warnings = append(warnings, fmt.Sprintf("meter %q: %s %s matched no pricing rule and the meter has no default price — not billed", meter.Key, agg.Quantity.String(), meter.Unit))
			unpriced = append(unpriced, unpricedBucket{MeterID: meterID, MeterKey: meter.Key, Mode: agg.AggregationMode, Quantity: agg.Quantity})
			continue
		}

		// Same resolution rule as the cycle scan and cancel finalize
		// (resolveRatedRule; ADR-070 pin-at-period-start): version and
		// override in force at the period open — pinAt, which IS `from`
		// for the operator preview and the threshold scan (which persists
		// these lines as a real invoice). This is
		// what makes preview == invoice hold across a mid-period
		// publish, and a threshold fire agree with the later close.
		// Both failures abort, as the cycle close does for the same rule: a
		// threshold fire persists these lines as a real invoice, and a
		// skipped line under-billed the fire (the bucket was then not on
		// the watermark either). A missing PINNED version is not "not set" —
		// usage is bound to it — so it fails like any other error.
		ratingRule, err := e.resolveRatedRule(ctx, tenantID, customerID, ratingRuleID, pinAt)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve pricing for meter %q rule %q: %w", meter.Key, ratingRuleID, err)
		}

		var amount int64
		if price != nil {
			amount, err = price(ratingRule, meterID+"|"+agg.RuleID, agg.AggregationMode, agg.Quantity)
		} else {
			amount, err = domain.ComputeAmountCents(ratingRule, agg.Quantity)
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("compute amount for meter %q rule %q: %w", meter.Key, ratingRule.RuleKey, err)
		}

		// Unit amount is amount / quantity, rounded to nearest cent. The
		// IsZero short-circuit above guarantees the divisor is non-zero.
		unitAmount := decimal.NewFromInt(amount).Div(agg.Quantity).RoundBank(0).IntPart()

		var dimMatch map[string]any
		if agg.RuleID != "" {
			if rule, ok := rulesByID[agg.RuleID]; ok && len(rule.DimensionMatch) > 0 {
				dimMatch = rule.DimensionMatch
			}
		}

		// Use the exact description the cycle scan persists (meter + dimension
		// match, e.g. "Tokens (claude-3.5-sonnet · input)") so a preview line
		// reads identically to the invoice_line_item it predicts. The quantity
		// lives in the Quantity field, not the description.
		desc := usageLineDescription(meter, rulesByID[agg.RuleID])

		lines = append(lines, PreviewLine{
			LineType:                 "usage",
			Description:              desc,
			MeterID:                  meterID,
			MeterUnit:                meter.Unit,
			RatingRuleVersionID:      ratingRule.ID,
			RuleKey:                  ratingRule.RuleKey,
			DimensionMatch:           dimMatch,
			Currency:                 ratingRule.Currency,
			Quantity:                 agg.Quantity,
			UnitAmountCents:          unitAmount,
			AmountCents:              amount,
			PricingMode:              string(ratingRule.Mode),
			AggregationMode:          agg.AggregationMode,
			NominalUnitAmountDecimal: nominalRate(ratingRule),
		})
	}

	return lines, warnings, unpriced, nil
}

// computePreviewTotals groups each line's amount by currency and emits
// one PreviewTotal per distinct currency. Order is insertion-stable
// (first currency seen wins position 0) so the response is deterministic
// across calls. Mirrors the customer-usage computeTotals helper.
func computePreviewTotals(lines []PreviewLine) []PreviewTotal {
	bucket := map[string]int64{}
	var order []string
	for _, line := range lines {
		if line.Currency == "" {
			continue
		}
		if _, ok := bucket[line.Currency]; !ok {
			order = append(order, line.Currency)
		}
		bucket[line.Currency] += line.AmountCents
	}
	out := make([]PreviewTotal, 0, len(order))
	for _, cur := range order {
		out = append(out, PreviewTotal{Currency: cur, AmountCents: bucket[cur]})
	}
	return out
}

// mapMeterAggregation translates the meter's stored aggregation string
// ("sum"/"count"/"max"/"last") to the AggregationMode the priority+claim
// resolver accepts. Duplicated from internal/usage/customer_usage.go
// rather than imported to keep the billing package's dependency graph
// flat (no cross-domain imports between peer packages — see CLAUDE.md
// "Architecture").
func mapMeterAggregation(agg string) domain.AggregationMode {
	switch agg {
	case "sum":
		return domain.AggSum
	case "count":
		return domain.AggCount
	case "max":
		return domain.AggMax
	case "last":
		return domain.AggLastDuringPeriod
	default:
		return domain.AggSum
	}
}
