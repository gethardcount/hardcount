package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// One rater (ADR-130 PR-1). Every path that prices a subscription period's
// usage prices it in rateUsageWindows: the period close (buildLineItems), the
// immediate cancel and the cross-interval plan swap (buildCancelLineItems),
// the preview and the spend-threshold evaluation (previewWindow), and the
// live credit balance (unbilledUsageCents). Each used to carry its own copy
// of the rating loop. The callers still choose the windows, the pass and the
// ladder, and still own their wording: a closer logs and counts what it left
// out, a preview warns. Customer usage (internal/usage) keeps its own loop
// until PR-4, which changes its output on purpose (ADR-130 §10).

// PriceSource is where the rater reads prices. It is defined here, by its
// only consumer. rulePrices implements it over the rating-rule tables; PR-4
// implements it over plan prices.
type PriceSource interface {
	// Meter is the meter whose usage is priced.
	Meter(ctx context.Context, tenantID, meterID string) (domain.Meter, error)
	// Rules are the meter's pricing rules. None means the meter's own
	// binding prices all of its usage.
	Rules(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error)
	// Price is the price in force at pin for usage bound to the pinned
	// rating-rule version (a pricing rule's or the meter's own), with the
	// customer's override in force at pin applied.
	Price(ctx context.Context, tenantID, customerID, pinned string, pin ratePin) (domain.RatingRuleVersion, error)
}

// ratePin is the instant a billing period's prices are read at: the start of
// the period being priced, for every window of it and every reader of it
// (ADR-070). testClockID names the subscription's time domain ("" is wall
// time). The rating-rule tables do not read it: their versions carry wall
// time and are compared with the pin as it is (ADR-070 decision 4). Plan
// prices read it (ADR-130 §7).
type ratePin struct {
	at          time.Time
	testClockID string
}

// pinFor is sub's pin for the period that starts at periodStart.
func pinFor(sub domain.Subscription, periodStart time.Time) ratePin {
	return ratePin{at: periodStart, testClockID: sub.TestClockID}
}

// rulePrices is the PriceSource over the rating-rule tables: the meter's own
// binding (meters.rating_rule_version_id), its pricing rules
// (meter_pricing_rules), their versions (rating_rule_versions) and the
// customer's overrides.
type rulePrices struct{ pricing PricingReader }

// prices is the engine's PriceSource.
func (e *Engine) prices() PriceSource { return rulePrices{pricing: e.pricing} }

func (p rulePrices) Meter(ctx context.Context, tenantID, meterID string) (domain.Meter, error) {
	return p.pricing.GetMeter(ctx, tenantID, meterID)
}

func (p rulePrices) Rules(ctx context.Context, tenantID, meterID string) ([]domain.MeterPricingRule, error) {
	return p.pricing.ListMeterPricingRulesByMeter(ctx, tenantID, meterID)
}

// Price resolves the version of the pinned binding's rule_key in force at the
// pin, then applies the customer's override in force at the same instant
// (ADR-070). One period, one price: a mid-close publish or override edit
// cannot mix prices inside one invoice.
//
// A missing override (errs.ErrNotFound) means list price. Any other failure,
// version resolution included, fails the rating; the caller's failure
// handling decides the retry (cycle billing: attemptSub, ADR-119). Treating an
// override error as absence once billed a negotiated customer at list price
// on a transient DB error.
func (p rulePrices) Price(ctx context.Context, tenantID, customerID, pinned string, pin ratePin) (domain.RatingRuleVersion, error) {
	linked, err := p.pricing.GetRatingRule(ctx, tenantID, pinned)
	if err != nil {
		return domain.RatingRuleVersion{}, fmt.Errorf("get rating rule %s: %w", pinned, err)
	}
	rule, err := p.pricing.GetRuleByKeyAsOf(ctx, tenantID, linked.RuleKey, pin.at)
	if err != nil {
		if !errors.Is(err, errs.ErrNotFound) {
			return domain.RatingRuleVersion{}, fmt.Errorf("resolve rule %q as of %s: %w", linked.RuleKey, pin.at.Format(time.RFC3339), err)
		}
		// The key has no active version (all archived) but a binding still
		// points at one: price at the pinned version rather than refuse to
		// bill usage the meter admitted.
		rule = linked
	}
	override, err := p.pricing.GetOverrideByKeyAsOf(ctx, tenantID, customerID, rule.RuleKey, pin.at)
	if err == nil {
		return override.ApplyTo(rule), nil
	}
	if errors.Is(err, errs.ErrNotFound) {
		return rule, nil
	}
	return domain.RatingRuleVersion{}, fmt.Errorf("resolve override for rule %q: %w", rule.RuleKey, err)
}

// ratePass says which of a window's buckets a pass prices (ADR-066 §4).
type ratePass int

const (
	// passEvery prices every bucket: the previews, the threshold evaluation
	// and the live balance.
	passEvery ratePass = iota
	// passClamped is a closer's pass over the watermark-clamped windows: every
	// bucket except a non-additive one the threshold invoice deferred.
	passClamped
	// passFull is a closer's pass over the unclamped windows after a
	// threshold invoice: only the non-additive buckets it deferred.
	passFull
)

// rateWindow is one stretch of a billing period over which one meter's usage
// is priced, and the pass that prices it.
type rateWindow struct {
	meterID string
	iv      usageInterval
	pass    ratePass
}

// usageReads reads one meter's usage buckets over one window.
type usageReads interface {
	windowBuckets(ctx context.Context, meterID string, meter domain.Meter, rules []domain.MeterPricingRule, iv usageInterval) ([]domain.RuleAggregation, error)
}

// windowBuckets is how the closers read a window. A meter with pricing rules is
// read per rule (AggregateByPricingRules). A meter without is read by its own
// aggregation, in the one read that covers every meter of the invoice for the
// window (AggregateForBillingPeriodByAgg); its total is the unclaimed bucket.
func (w *windowUsage) windowBuckets(ctx context.Context, meterID string, meter domain.Meter, rules []domain.MeterPricingRule, iv usageInterval) ([]domain.RuleAggregation, error) {
	if len(rules) > 0 {
		return w.ruleBuckets(ctx, meterID, iv)
	}
	total, err := w.meterTotal(ctx, meterID, iv)
	if err != nil {
		return nil, err
	}
	return []domain.RuleAggregation{{AggregationMode: mapMeterAggregation(meter.Aggregation), Quantity: total}}, nil
}

// ruleReads is how the previews and the live balance read a window: every
// meter through AggregateByPricingRules, rules or not.
type ruleReads struct {
	usage                UsageAggregator
	tenantID, customerID string
}

func (r ruleReads) windowBuckets(ctx context.Context, meterID string, meter domain.Meter, _ []domain.MeterPricingRule, iv usageInterval) ([]domain.RuleAggregation, error) {
	aggs, err := r.usage.AggregateByPricingRules(ctx, r.tenantID, r.customerID, meterID, mapMeterAggregation(meter.Aggregation), iv.start, iv.end)
	if err != nil {
		return nil, fmt.Errorf("aggregate by pricing rules: %w", err)
	}
	return aggs, nil
}

// rateSite is who rates, for the wording of a failure. Each caller keeps the
// wording it had before the rater: the period close (the zero value), the
// cancel ("… on cancel"), or a preview, which names its window.
type rateSite struct {
	cancel  bool
	preview func(w rateWindow, err error) error
}

// fail is how a failure inside window w reads.
func (s rateSite) fail(w rateWindow, err error) error {
	if s.preview != nil {
		return s.preview(w, err)
	}
	return err
}

// on is the cancel's suffix on a failure that names the meter.
func (s rateSite) on() string {
	if s.cancel {
		return " on cancel"
	}
	return ""
}

// rater rates one subscription's usage windows.
type rater struct {
	prices               PriceSource
	reads                usageReads
	site                 rateSite
	tenantID, customerID string
}

// ratedBucket is one usage bucket of one window: priced, or left out because
// no price binds it (unpriced) or it is below zero after corrections.
type ratedBucket struct {
	w        rateWindow
	meter    domain.Meter
	hasRules bool                    // the meter is priced by pricing rules
	rule     domain.MeterPricingRule // the rule that claimed the bucket; zero for the unclaimed bucket
	agg      domain.RuleAggregation
	priced   bool
	price    domain.RatingRuleVersion // in force at the pin, override applied
	amount   int64
}

// meterPricing is one meter's pricing configuration, read once per rating.
type meterPricing struct {
	meter domain.Meter
	rules map[string]domain.MeterPricingRule // by pricing rule id
	list  []domain.MeterPricingRule
}

// rateUsageWindows prices each window's usage buckets in order, at pin.
//
// ladder continues each bucket's tiers from what a threshold invoice already
// billed this period (ADR-066 amendment 2026-10-11); nil prices each bucket
// on its own. A closer's pass (passClamped, passFull) routes each bucket by
// ladder's watermark and checks, once per meter, that the watermark can seed
// each ladder; a closer always passes a ladder.
//
// It returns the buckets in order. On a failure it returns the buckets
// before it too, so a closer logs what it left out up to there.
func (r rater) rateUsageWindows(ctx context.Context, windows []rateWindow, pin ratePin, ladder *usageLadder) ([]ratedBucket, error) {
	var out []ratedBucket
	meters := map[string]meterPricing{}
	seeded := map[string]bool{}
	for _, w := range windows {
		mp, ok := meters[w.meterID]
		if !ok {
			var err error
			if mp, err = r.meterPricing(ctx, w.meterID); err != nil {
				return out, r.site.fail(w, err)
			}
			meters[w.meterID] = mp
		}
		if w.pass != passEvery && !seeded[w.meterID] {
			seeded[w.meterID] = true
			if err := checkLadderSeeds(ctx, r.prices, r.tenantID, r.customerID, w.meterID, mp.meter, mp.list, pin, ladder); err != nil {
				return out, r.site.fail(w, err)
			}
		}
		// A meter without pricing rules has one bucket, whose mode and
		// binding are the meter's own: it is routed before it is read, so a
		// closer's full pass reads the whole period only for a bucket the
		// threshold invoice deferred (ADR-066 §4).
		if w.pass != passEvery && len(mp.list) == 0 && !routedHere(w, ladder, mapMeterAggregation(mp.meter.Aggregation), mp.meter.RatingRuleVersionID) {
			continue
		}
		aggs, err := r.reads.windowBuckets(ctx, w.meterID, mp.meter, mp.list, w.iv)
		if err != nil {
			if r.site.cancel {
				err = fmt.Errorf("cancel: %w", err)
			}
			return out, r.site.fail(w, err)
		}
		for _, agg := range aggs {
			b, ok, err := r.rateBucket(ctx, w, mp, agg, pin, ladder)
			if err != nil {
				return out, r.site.fail(w, err)
			}
			if ok {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// meterPricing reads the meter and its pricing rules.
func (r rater) meterPricing(ctx context.Context, meterID string) (meterPricing, error) {
	meter, err := r.prices.Meter(ctx, r.tenantID, meterID)
	if err != nil {
		if r.site.preview != nil {
			return meterPricing{}, fmt.Errorf("get meter: %w", err)
		}
		return meterPricing{}, fmt.Errorf("get meter %s%s: %w", meterID, r.site.on(), err)
	}
	list, err := r.prices.Rules(ctx, r.tenantID, meterID)
	if err != nil {
		if r.site.preview != nil {
			return meterPricing{}, fmt.Errorf("list pricing rules: %w", err)
		}
		return meterPricing{}, fmt.Errorf("list pricing rules for meter %s%s: %w", meterID, r.site.on(), err)
	}
	rules := make(map[string]domain.MeterPricingRule, len(list))
	for _, rule := range list {
		rules[rule.ID] = rule
	}
	return meterPricing{meter: meter, rules: rules, list: list}, nil
}

// rateBucket prices one bucket of window w. ok is false for a bucket that is
// not this pass's to price: empty, or routed to the closer's other pass.
func (r rater) rateBucket(ctx context.Context, w rateWindow, mp meterPricing, agg domain.RuleAggregation, pin ratePin, ladder *usageLadder) (ratedBucket, bool, error) {
	qty := agg.Quantity
	if qty.IsZero() {
		return ratedBucket{}, false, nil
	}
	b := ratedBucket{w: w, meter: mp.meter, hasRules: len(mp.list) > 0, rule: mp.rules[agg.RuleID], agg: agg}
	// The bucket's binding: its pricing rule's version, or the meter's own
	// for the unclaimed bucket.
	pinned := agg.RatingRuleVersionID
	if pinned == "" {
		pinned = mp.meter.RatingRuleVersionID
	}
	// A closer's pass routing (ADR-066 §4), per bucket: a non-additive bucket
	// the threshold invoice deferred (no line on it) bills only on the full
	// pass, over the unclamped window; every other bucket only on the clamped
	// pass. It runs before the checks below so a bucket left out is reported
	// on one pass only (#981). With no watermark nothing is deferred.
	if w.pass != passEvery && !routedHere(w, ladder, agg.AggregationMode, pinned) {
		return ratedBucket{}, false, nil
	}
	// Corrections larger than the bucket's usage in this window: it counts as
	// 0 and the rest is billed (#1193, net_negative.go).
	if qty.IsNegative() {
		return b, true, nil
	}
	// No rule claimed the usage and the meter has no binding of its own: no
	// price (#981, ADR-096).
	if pinned == "" {
		return b, true, nil
	}
	// The price in force at the pin, the period start, for every window and
	// every caller (ADR-070): a preview equals the invoice across a
	// mid-period publish, and a threshold invoice agrees with the later close.
	// A failure fails the rating, here as in the close: the threshold fire
	// persists these lines as an invoice, and a skipped bucket would be
	// billed by nobody. A missing pinned version is such a failure too: usage
	// is bound to it.
	price, err := r.prices.Price(ctx, r.tenantID, r.customerID, pinned, pin)
	if err != nil {
		switch {
		case r.site.preview != nil:
			return ratedBucket{}, false, fmt.Errorf("resolve pricing for meter %q rule %q: %w", mp.meter.Key, pinned, err)
		case b.hasRules:
			return ratedBucket{}, false, fmt.Errorf("resolve pricing for meter %s rule %s%s: %w", w.meterID, pinned, r.site.on(), err)
		default:
			return ratedBucket{}, false, fmt.Errorf("resolve pricing for meter %s%s: %w", w.meterID, r.site.on(), err)
		}
	}
	var amount int64
	if ladder != nil {
		amount, err = ladder.price(price, w.meterID, w.meterID+"|"+agg.RuleID, agg.AggregationMode, qty)
	} else {
		amount, err = domain.ComputeAmountCents(price, qty)
	}
	if err != nil {
		switch {
		case r.site.preview != nil:
			return ratedBucket{}, false, fmt.Errorf("compute amount for meter %q rule %q: %w", mp.meter.Key, price.RuleKey, err)
		case b.hasRules:
			return ratedBucket{}, false, fmt.Errorf("compute amount for meter %s rule %s%s: %w", w.meterID, pinned, r.site.on(), err)
		default:
			return ratedBucket{}, false, fmt.Errorf("compute amount for meter %s%s: %w", w.meterID, r.site.on(), err)
		}
	}
	b.priced, b.price, b.amount = true, price, amount
	return b, true, nil
}

// routedHere reports whether a closer's pass w prices a bucket of mode bound
// to pinned: a bucket the threshold invoice deferred only on the full pass,
// every other bucket only on the clamped pass.
func routedHere(w rateWindow, ladder *usageLadder, mode domain.AggregationMode, pinned string) bool {
	return ladder.wm.deferredBucket(mode, w.meterID, pinned) == (w.pass == passFull)
}

// unitAmount is the line's amount over its quantity, rounded to a whole
// cent. A rated bucket's quantity is never zero.
func (b ratedBucket) unitAmount() int64 {
	return decimal.NewFromInt(b.amount).Div(b.agg.Quantity).RoundBank(0).IntPart()
}

// invoiceLine is a priced bucket as a closer's invoice line, in the invoice
// currency, over the bucket's window. suffix ends the description: it says
// why a cancel's or a swap's window ended early.
func (b ratedBucket) invoiceLine(currency, suffix string) domain.InvoiceLineItem {
	start, end := b.w.iv.start, b.w.iv.end
	return domain.InvoiceLineItem{
		LineType:                 domain.LineTypeUsage,
		MeterID:                  b.w.meterID,
		MeterUnit:                b.meter.Unit,
		Description:              usageLineDescription(b.meter, b.rule) + suffix,
		Quantity:                 b.agg.Quantity.IntPart(),
		QuantityDecimal:          b.agg.Quantity,
		UnitAmountCents:          b.unitAmount(),
		AmountCents:              b.amount,
		TotalAmountCents:         b.amount,
		Currency:                 currency,
		PricingMode:              string(b.price.Mode),
		RatingRuleVersionID:      b.price.ID,
		NominalUnitAmountDecimal: nominalRate(b.price),
		BillingPeriodStart:       &start,
		BillingPeriodEnd:         &end,
	}
}

// noteLeftOut is a closer's record of a bucket it left out: a WARN under the
// closer's prefix ("cycle", "cancel"), and the quantity added to belowZero
// (written on the closer's transaction) or to unpriced (counted after it
// commits).
func (b ratedBucket) noteLeftOut(prefix string, sub domain.Subscription, unpriced unpricedUsage, belowZero netNegativeUsage) {
	qty := b.agg.Quantity
	switch {
	case qty.IsNegative():
		slog.Warn(prefix+": usage below zero after corrections — billed as 0",
			"meter", b.meter.Key, "quantity", qty.String(), "subscription_id", sub.ID)
		belowZero.add(b.w.meterID, b.meter.Key, b.meter.Name, qty, b.w.iv.start, b.w.iv.end)
	case b.hasRules:
		slog.Warn(prefix+": usage matched no rating rule and meter has no default binding — not billed",
			"meter", b.meter.Key, "quantity", qty.String(), "subscription_id", sub.ID)
		unpriced.add(b.w.meterID, qty)
	default:
		// The plan-attach guard (ADR-096) blocks this at authoring, so
		// reaching it means a rule was unbound out from under a live sub.
		slog.Warn(prefix+": usage on a meter with no rating rule — not billed (unpriced meter on plan; ADR-096)",
			"meter", b.meter.Key, "quantity", qty.String(), "subscription_id", sub.ID)
		unpriced.add(b.w.meterID, qty)
	}
}

// previewLine is a priced bucket as a preview line. Its description is the
// one the closers write on the invoice line it predicts. Its currency is the
// price's; the close-mirroring preview replaces it with the invoice's.
func (b ratedBucket) previewLine() PreviewLine {
	var dimMatch map[string]any
	if len(b.rule.DimensionMatch) > 0 {
		dimMatch = b.rule.DimensionMatch
	}
	return PreviewLine{
		LineType:                 "usage",
		Description:              usageLineDescription(b.meter, b.rule),
		MeterID:                  b.w.meterID,
		MeterUnit:                b.meter.Unit,
		RatingRuleVersionID:      b.price.ID,
		RuleKey:                  b.price.RuleKey,
		DimensionMatch:           dimMatch,
		Currency:                 b.price.Currency,
		Quantity:                 b.agg.Quantity,
		UnitAmountCents:          b.unitAmount(),
		AmountCents:              b.amount,
		PricingMode:              string(b.price.Mode),
		AggregationMode:          b.agg.AggregationMode,
		NominalUnitAmountDecimal: nominalRate(b.price),
	}
}

// previewLeftOut is a bucket left out, as a preview's warning (for people)
// and as data (for the threshold fire, which reports it).
func (b ratedBucket) previewLeftOut() (string, leftOutBucket) {
	qty := b.agg.Quantity
	out := leftOutBucket{MeterID: b.w.meterID, MeterKey: b.meter.Key, MeterName: b.meter.Name, Mode: b.agg.AggregationMode, Quantity: qty}
	if qty.IsNegative() {
		// A legitimate state, not a pricing error: corrections can put a
		// bucket's total below zero until new usage overtakes it.
		out.BelowZero = true
		return fmt.Sprintf("meter %q: net-negative usage so far (%s) after corrections — counted as 0", b.meter.Key, qty.String()), out
	}
	// The quantity is what the operator needs to bill it by hand (#981).
	return fmt.Sprintf("meter %q: %s %s matched no pricing rule and the meter has no default price — not billed", b.meter.Key, qty.String(), b.meter.Unit), out
}

// rateCloseUsage prices a closer's usage: each meter over its clamped
// windows, then, after a threshold invoice, over its full windows for the
// non-additive buckets that invoice deferred (ADR-066 §4), with one tier
// ladder continuing from that invoice. The period close and the cancel share
// it. Each priced bucket is a line in currency whose description ends with
// suffix; each bucket left out is logged under logPrefix and added to
// unpriced or belowZero.
func (e *Engine) rateCloseUsage(ctx context.Context, sub domain.Subscription, reads *windowUsage, site rateSite, clamped, full map[string][]usageInterval, wm thresholdWatermark, periodStart time.Time, currency, suffix, logPrefix string, unpriced unpricedUsage, belowZero netNegativeUsage) ([]domain.InvoiceLineItem, int64, error) {
	// Every meter with a billable window. When a watermark exists, a meter
	// whose clamped windows vanished (all its usage inside the threshold
	// invoice's window) can still owe a full-window bill for a deferred
	// non-additive bucket, so the full map's meters count too.
	meterIDs := make(map[string]struct{}, len(clamped))
	for mid := range clamped {
		meterIDs[mid] = struct{}{}
	}
	for mid := range full {
		meterIDs[mid] = struct{}{}
	}
	// Meters in id order, so a failure names the same meter on every run
	// and every retry.
	var windows []rateWindow
	for _, meterID := range slices.Sorted(maps.Keys(meterIDs)) {
		for _, iv := range mergeUsageIntervals(clamped[meterID]) {
			windows = append(windows, rateWindow{meterID: meterID, iv: iv, pass: passClamped})
		}
		if wm.exists() {
			for _, iv := range mergeUsageIntervals(full[meterID]) {
				windows = append(windows, rateWindow{meterID: meterID, iv: iv, pass: passFull})
			}
		}
	}
	r := rater{prices: e.prices(), reads: reads, site: site, tenantID: sub.TenantID, customerID: sub.CustomerID}
	buckets, err := r.rateUsageWindows(ctx, windows, pinFor(sub, periodStart), newUsageLadder(wm))
	var lines []domain.InvoiceLineItem
	var subtotal int64
	for _, b := range buckets {
		if !b.priced {
			b.noteLeftOut(logPrefix, sub, unpriced, belowZero)
			continue
		}
		line := b.invoiceLine(currency, suffix)
		lines = append(lines, line)
		subtotal += line.AmountCents
	}
	if err != nil {
		return nil, 0, err
	}
	return lines, subtotal, nil
}

// ratePreviewUsage prices a preview's windows, every bucket, at the period
// start. A failure names the window's meter.
func (e *Engine) ratePreviewUsage(ctx context.Context, sub domain.Subscription, windows []rateWindow, periodStart time.Time, ladder *usageLadder) ([]PreviewLine, []string, []leftOutBucket, error) {
	site := rateSite{preview: func(w rateWindow, err error) error {
		return fmt.Errorf("preview meter %q: %w", w.meterID, err)
	}}
	r := rater{prices: e.prices(), reads: ruleReads{usage: e.usage, tenantID: sub.TenantID, customerID: sub.CustomerID}, site: site, tenantID: sub.TenantID, customerID: sub.CustomerID}
	buckets, err := r.rateUsageWindows(ctx, windows, pinFor(sub, periodStart), ladder)
	if err != nil {
		return nil, nil, nil, err
	}
	var (
		lines    []PreviewLine
		warnings []string
		leftOut  []leftOutBucket
	)
	for _, b := range buckets {
		if b.priced {
			lines = append(lines, b.previewLine())
			continue
		}
		warning, lo := b.previewLeftOut()
		warnings = append(warnings, warning)
		leftOut = append(leftOut, lo)
	}
	return lines, warnings, leftOut, nil
}

// rateTap is the hook of the same-build differential test
// (legacy_rater_test.go, ADR-130 PR-1). When a test binary sets it, each of
// the four rating entry points hands it the call; it rates the call through
// this file and through the old loops that test keeps, and fails the binary
// when they differ. Production never sets it. PR-4 deletes it with the old
// loops.
var rateTap rateTapper

// rateTapper is implemented only by the differential test.
type rateTapper interface {
	buildLineItems(ctx context.Context, e *Engine, sub domain.Subscription, now, periodStart, periodEnd time.Time, plans map[string]domain.Plan, invoiceCurrency string, billingLoc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage) ([]domain.InvoiceLineItem, int64, *time.Time, error)
	buildCancelLineItems(ctx context.Context, e *Engine, sub domain.Subscription, wm thresholdWatermark, plans map[string]domain.Plan, invoiceCurrency string, periodStart, canceledAt time.Time, billingLoc *time.Location, unpriced unpricedUsage, belowZero netNegativeUsage, usageSuffix string) ([]domain.InvoiceLineItem, int64, error)
	previewWindow(ctx context.Context, e *Engine, sub domain.Subscription, periodStart, periodEnd time.Time, asClose bool) (PreviewResult, error)
	unbilledUsageCents(ctx context.Context, e *Engine, sub domain.Subscription) (int64, string, error)
}

// rateTapOff marks a call the tap made itself, so it is not tapped again.
type rateTapOff struct{}

// tapped reports whether this call goes to rateTap.
func tapped(ctx context.Context) bool {
	return rateTap != nil && ctx.Value(rateTapOff{}) == nil
}
