package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// One rater (ADR-130 PR-1). Every path that prices a subscription period's
// usage prices it in rateUsageWindows: the period close (buildLineItems), the
// immediate cancel and the cross-interval plan swap (buildCancelLineItems),
// the previews and the spend-threshold invoice (previewWindow,
// thresholdLines), the live credit balance (unbilledUsageCents) and customer
// usage (RateCurrentPeriod, RateWindow). The callers choose the windows, the
// pass and the ladder, and own their wording: a closer logs and counts what
// it left out, a preview warns.
//
// A window is one stretch of a billing period during which a subscription
// was on one plan, for one meter that plan prices (ADR-130 §9). Each window
// is priced by its own plan's prices, so after a mid-period swap between two
// plans that price one meter the period bills two lines, and tiers restart
// in each window.

// PriceSource is where the rater reads prices. It is defined here, by its
// only consumer, and implemented over plan prices (planPrices).
type PriceSource interface {
	// Meter is the meter whose usage is priced.
	Meter(ctx context.Context, tenantID, meterID string) (domain.Meter, error)
	// PlanPrices are the plan's prices that take part at the pin, each with
	// its version in force then, in claim order per meter (ADR-130 §4).
	PlanPrices(ctx context.Context, tenantID, planID string, pin ratePin) ([]domain.PlanPrice, error)
	// Price is price with the customer's override in force at the pin
	// applied.
	Price(ctx context.Context, tenantID, customerID string, price domain.PlanPrice, pin ratePin) (domain.PlanPrice, error)
}

// ratePin is the instant a billing period's prices are read at: the start of
// the period being priced, for every window of it and every reader of it
// (ADR-130 §4 step 3). testClockID names the subscription's time domain (""
// is wall time): a test-clock subscription compares its simulated pin with
// each version's stamp for its clock (ADR-130 §7).
type ratePin struct {
	at          time.Time
	testClockID string
}

// pinFor is sub's pin for the period that starts at periodStart.
func pinFor(sub domain.Subscription, periodStart time.Time) ratePin {
	return ratePin{at: periodStart, testClockID: sub.TestClockID}
}

// planPrices is the PriceSource over plan prices. It reads each plan's
// prices at a pin once per rating, so every window of the plan, and the
// window builder, see one price set.
type planPrices struct {
	pricing PricingReader
	cache   map[planPin][]domain.PlanPrice
}

type planPin struct {
	planID string
	pin    ratePin
}

// prices is a new PriceSource for one rating.
func (e *Engine) prices() *planPrices {
	return &planPrices{pricing: e.pricing, cache: map[planPin][]domain.PlanPrice{}}
}

func (p *planPrices) Meter(ctx context.Context, tenantID, meterID string) (domain.Meter, error) {
	return p.pricing.GetMeter(ctx, tenantID, meterID)
}

func (p *planPrices) PlanPrices(ctx context.Context, tenantID, planID string, pin ratePin) ([]domain.PlanPrice, error) {
	k := planPin{planID: planID, pin: pin}
	if got, ok := p.cache[k]; ok {
		return got, nil
	}
	got, err := p.pricing.PlanPricesAt(ctx, tenantID, planID, pin.at, pin.testClockID)
	if err != nil {
		return nil, fmt.Errorf("prices of plan %s at %s: %w", planID, pin.at.Format(time.RFC3339), err)
	}
	p.cache[k] = got
	return got, nil
}

// Price applies the customer's override in force at the pin (ADR-130 §4
// step 8). A missing override (errs.ErrNotFound) means list price. Any other
// failure fails the rating; the caller's failure handling decides the retry.
// Treating an override error as absence once billed a negotiated customer at
// list price on a transient DB error.
func (p *planPrices) Price(ctx context.Context, tenantID, customerID string, price domain.PlanPrice, pin ratePin) (domain.PlanPrice, error) {
	o, err := p.pricing.GetOverrideAt(ctx, tenantID, customerID, price.ID, pin.at)
	if err == nil {
		return o.ApplyTo(price), nil
	}
	if errors.Is(err, errs.ErrNotFound) {
		return price, nil
	}
	return domain.PlanPrice{}, fmt.Errorf("resolve override for price %s: %w", price.ID, err)
}

// pricesOn is the prices of meterID among prices, in their order.
func pricesOn(prices []domain.PlanPrice, meterID string) []domain.PlanPrice {
	var out []domain.PlanPrice
	for _, p := range prices {
		if p.MeterID == meterID {
			out = append(out, p)
		}
	}
	return out
}

// planMetersAt is, for each plan, the meters it prices at the pin: the
// meters with a price that takes part then (ADR-130 §4 step 4). A meter
// whose prices were all removed before the pin is not on the plan for that
// period, and neither is a meter whose first price comes later.
func planMetersAt(ctx context.Context, src PriceSource, tenantID string, plans map[string]domain.Plan, pin ratePin) (map[string][]string, error) {
	out := make(map[string][]string, len(plans))
	for _, planID := range slices.Sorted(maps.Keys(plans)) {
		prices, err := src.PlanPrices(ctx, tenantID, planID, pin)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range prices {
			if !seen[p.MeterID] {
				seen[p.MeterID] = true
				out[planID] = append(out[planID], p.MeterID)
			}
		}
		sort.Strings(out[planID])
	}
	return out, nil
}

// readMeters reads every meter of planMeters once. An unreadable meter is an
// error (retried per ADR-119): defaulting its aggregation to "sum" billed a
// max or last meter as its total.
func (e *Engine) readMeters(ctx context.Context, tenantID string, planMeters map[string][]string) (map[string]domain.Meter, error) {
	meters := map[string]domain.Meter{}
	for _, planID := range slices.Sorted(maps.Keys(planMeters)) {
		for _, meterID := range planMeters[planID] {
			if _, ok := meters[meterID]; ok {
				continue
			}
			m, err := e.pricing.GetMeter(ctx, tenantID, meterID)
			if err != nil {
				return nil, fmt.Errorf("get meter %s aggregation: %w", meterID, err)
			}
			meters[meterID] = m
		}
	}
	return meters, nil
}

// ratePass says which of a window's buckets a pass prices (ADR-066 §4).
type ratePass int

const (
	// passEvery prices every bucket: the previews, the threshold invoice,
	// the live balance and customer usage.
	passEvery ratePass = iota
	// passClamped is a closer's pass over the watermark-clamped windows: every
	// bucket except a non-additive one the threshold invoice deferred.
	passClamped
	// passFull is a closer's pass over the unclamped windows after a
	// threshold invoice: only the non-additive buckets it deferred.
	passFull
)

// rateWindow is one stretch of a billing period over which one plan's prices
// for one meter price its usage, and the pass that prices it.
type rateWindow struct {
	planID  string
	meterID string
	iv      usageInterval
	pass    ratePass
}

// usageReads reads one meter's usage buckets over one window.
type usageReads interface {
	windowBuckets(ctx context.Context, w rateWindow, meter domain.Meter, prices []domain.PlanPrice) ([]domain.ClaimedUsage, error)
}

// catchAllOnly reports whether prices is one price that takes every event
// of its meter: its bucket is the meter's whole usage.
func catchAllOnly(prices []domain.PlanPrice) bool {
	return len(prices) == 1 && len(prices[0].DimensionMatch) == 0
}

// windowBuckets is how the closers read a window. A meter priced by one
// catch-all price is read by its own aggregation, in the one read that covers
// every meter of the invoice for the window (AggregateForBillingPeriodByAgg);
// its total is that price's bucket. Any other meter is read per price
// (AggregateClaims). Both read under the meter's one aggregation (ADR-130 §2).
func (w *windowUsage) windowBuckets(ctx context.Context, rw rateWindow, meter domain.Meter, prices []domain.PlanPrice) ([]domain.ClaimedUsage, error) {
	if catchAllOnly(prices) {
		total, err := w.meterTotal(ctx, rw.meterID, rw.iv)
		if err != nil {
			return nil, err
		}
		return []domain.ClaimedUsage{{PriceID: prices[0].ID, Quantity: total}}, nil
	}
	return w.claimBuckets(ctx, rw.meterID, meter, prices, rw.iv)
}

// claimReads is how the previews, the live balance and customer usage read
// a window: every meter through AggregateClaims.
type claimReads struct {
	usage                UsageAggregator
	tenantID, customerID string
}

func (r claimReads) windowBuckets(ctx context.Context, w rateWindow, meter domain.Meter, prices []domain.PlanPrice) ([]domain.ClaimedUsage, error) {
	aggs, err := r.usage.AggregateClaims(ctx, r.tenantID, r.customerID, w.meterID, meter.Aggregation, domain.ClaimsOf(prices), w.iv.start, w.iv.end)
	if err != nil {
		return nil, fmt.Errorf("aggregate by price: %w", err)
	}
	return aggs, nil
}

// rateSite is who rates, for the wording of a failure: the period close
// (the zero value), the cancel ("… on cancel"), or a preview, which names
// its window.
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
// no price of the window's plan takes it (unpriced) or it is below zero after
// corrections.
type ratedBucket struct {
	w      rateWindow
	meter  domain.Meter
	agg    domain.ClaimedUsage
	priced bool
	price  domain.PlanPrice // in force at the pin, override applied
	amount int64
}

// rateUsageWindows prices each window's usage buckets in order, at pin.
//
// ladder continues a bucket's tiers from what a threshold invoice already
// billed this period (ADR-066 amendment 2026-10-11), keyed by price id; nil
// prices each bucket on its own. A closer's pass (passClamped, passFull)
// routes each bucket by ladder's watermark; a closer always passes a ladder.
//
// It returns the buckets in order. On a failure it returns the buckets
// before it too, so a closer logs what it left out up to there.
func (r rater) rateUsageWindows(ctx context.Context, windows []rateWindow, pin ratePin, ladder *usageLadder) ([]ratedBucket, error) {
	var out []ratedBucket
	meters := map[string]domain.Meter{}
	for _, w := range windows {
		meter, ok := meters[w.meterID]
		if !ok {
			var err error
			if meter, err = r.prices.Meter(ctx, r.tenantID, w.meterID); err != nil {
				if r.site.preview != nil {
					return out, r.site.fail(w, fmt.Errorf("get meter: %w", err))
				}
				return out, fmt.Errorf("get meter %s%s: %w", w.meterID, r.site.on(), err)
			}
			meters[w.meterID] = meter
		}
		all, err := r.prices.PlanPrices(ctx, r.tenantID, w.planID, pin)
		if err != nil {
			return out, r.site.fail(w, err)
		}
		prices := pricesOn(all, w.meterID)
		// A meter priced by one catch-all price has one bucket, whose price
		// is known before it is read: it is routed first, so a closer's full
		// pass reads the whole period only for a bucket the threshold invoice
		// deferred (ADR-066 §4).
		if w.pass != passEvery && catchAllOnly(prices) && !routedHere(w, ladder, meter.Aggregation, prices[0].ID) {
			continue
		}
		aggs, err := r.reads.windowBuckets(ctx, w, meter, prices)
		if err != nil {
			if r.site.cancel {
				err = fmt.Errorf("cancel: %w", err)
			}
			return out, r.site.fail(w, err)
		}
		byID := make(map[string]domain.PlanPrice, len(prices))
		for _, p := range prices {
			byID[p.ID] = p
		}
		for _, agg := range aggs {
			b, ok, err := r.rateBucket(ctx, w, meter, byID, agg, pin, ladder)
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

// rateBucket prices one bucket of window w. ok is false for a bucket that is
// not this pass's to price: empty, or routed to the closer's other pass.
func (r rater) rateBucket(ctx context.Context, w rateWindow, meter domain.Meter, prices map[string]domain.PlanPrice, agg domain.ClaimedUsage, pin ratePin, ladder *usageLadder) (ratedBucket, bool, error) {
	qty := agg.Quantity
	if qty.IsZero() {
		return ratedBucket{}, false, nil
	}
	b := ratedBucket{w: w, meter: meter, agg: agg}
	// A closer's pass routing (ADR-066 §4), per bucket: a non-additive bucket
	// the threshold invoice deferred (no line on it) bills only on the full
	// pass, over the unclamped window; every other bucket only on the clamped
	// pass. It runs before the checks below so a bucket left out is reported
	// on one pass only (#981). With no watermark nothing is deferred.
	if w.pass != passEvery && !routedHere(w, ladder, meter.Aggregation, agg.PriceID) {
		return ratedBucket{}, false, nil
	}
	// Corrections larger than the bucket's usage in this window: it counts as
	// 0 and the rest is billed (#1193, net_negative.go).
	if qty.IsNegative() {
		return b, true, nil
	}
	// No price of the plan took the usage: no price (ADR-130 §4 step 7).
	if agg.PriceID == "" {
		return b, true, nil
	}
	listed, ok := prices[agg.PriceID]
	if !ok {
		return ratedBucket{}, false, fmt.Errorf("usage of meter %s is claimed by price %s, which plan %s does not have at the period start", w.meterID, agg.PriceID, w.planID)
	}
	// The price in force at the pin, the period start, for every window and
	// every caller: a preview equals the invoice across a mid-period edit,
	// and a threshold invoice agrees with the later close. A failure fails
	// the rating, here as in the close: the threshold fire persists these
	// lines as an invoice, and a skipped bucket would be billed by nobody.
	price, err := r.prices.Price(ctx, r.tenantID, r.customerID, listed, pin)
	if err != nil {
		if r.site.preview != nil {
			return ratedBucket{}, false, fmt.Errorf("resolve pricing for meter %q price %s: %w", meter.EventName, listed.ID, err)
		}
		return ratedBucket{}, false, fmt.Errorf("resolve pricing for meter %s price %s%s: %w", w.meterID, listed.ID, r.site.on(), err)
	}
	var amount int64
	if ladder != nil {
		amount, err = ladder.price(price, w, meter.Aggregation, qty)
	} else {
		amount, err = domain.ComputeAmountCents(price.PriceShape, qty)
	}
	if err != nil {
		if r.site.preview != nil {
			return ratedBucket{}, false, fmt.Errorf("compute amount for meter %q price %s: %w", meter.EventName, price.ID, err)
		}
		return ratedBucket{}, false, fmt.Errorf("compute amount for meter %s price %s%s: %w", w.meterID, price.ID, r.site.on(), err)
	}
	b.priced, b.price, b.amount = true, price, amount
	return b, true, nil
}

// routedHere reports whether a closer's pass w prices a bucket of a meter
// aggregated by aggregation, claimed by priceID ("" for the unclaimed
// bucket): a bucket the threshold invoice deferred only on the full pass,
// every other bucket only on the clamped pass.
func routedHere(w rateWindow, ladder *usageLadder, aggregation, priceID string) bool {
	return ladder.wm.deferredBucket(aggregation, w.meterID, priceID) == (w.pass == passFull)
}

// unitAmount is the line's amount over its quantity, rounded to a whole
// cent. A rated bucket's quantity is never zero.
func (b ratedBucket) unitAmount() int64 {
	return decimal.NewFromInt(b.amount).Div(b.agg.Quantity).RoundBank(0).IntPart()
}

// lineMatch is a price's dimension_match as a line carries it: nil for the
// catch-all, so a line that names no labels has none.
func lineMatch(p domain.PlanPrice) map[string]string {
	if len(p.DimensionMatch) == 0 {
		return nil
	}
	return p.DimensionMatch
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
		Description:              usageLineDescription(b.meter, b.price) + suffix,
		Quantity:                 b.agg.Quantity.IntPart(),
		QuantityDecimal:          b.agg.Quantity,
		UnitAmountCents:          b.unitAmount(),
		AmountCents:              b.amount,
		TotalAmountCents:         b.amount,
		Currency:                 currency,
		PricingMode:              string(b.price.Mode),
		PriceID:                  b.price.ID,
		DimensionMatch:           lineMatch(b.price),
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
	if qty.IsNegative() {
		slog.Warn(prefix+": usage below zero after corrections — billed as 0",
			"meter", b.meter.EventName, "quantity", qty.String(), "subscription_id", sub.ID)
		belowZero.add(b.w.meterID, b.meter.EventName, b.meter.Name, qty, b.w.iv.start, b.w.iv.end)
		return
	}
	slog.Warn(prefix+": usage matched no price of its plan — not billed",
		"meter", b.meter.EventName, "plan_id", b.w.planID, "quantity", qty.String(), "subscription_id", sub.ID)
	unpriced.add(b.w.meterID, qty)
}

// previewLine is a priced bucket as a preview line. Its description is the
// one the closers write on the invoice line it predicts.
func (b ratedBucket) previewLine(currency string) PreviewLine {
	return PreviewLine{
		LineType:                 "usage",
		Description:              usageLineDescription(b.meter, b.price),
		MeterID:                  b.w.meterID,
		MeterUnit:                b.meter.Unit,
		PriceID:                  b.price.ID,
		PlanID:                   b.w.planID,
		DimensionMatch:           lineMatch(b.price),
		Currency:                 currency,
		Quantity:                 b.agg.Quantity,
		UnitAmountCents:          b.unitAmount(),
		AmountCents:              b.amount,
		PricingMode:              string(b.price.Mode),
		Aggregation:              b.meter.Aggregation,
		NominalUnitAmountDecimal: nominalRate(b.price),
		windowStart:              b.w.iv.start,
		windowEnd:                b.w.iv.end,
	}
}

// previewLeftOut is a bucket left out, as a preview's warning (for people)
// and as data (for the threshold fire, which reports it).
func (b ratedBucket) previewLeftOut() (string, leftOutBucket) {
	qty := b.agg.Quantity
	out := leftOutBucket{MeterID: b.w.meterID, EventName: b.meter.EventName, MeterName: b.meter.Name, Aggregation: b.meter.Aggregation, Quantity: qty}
	if qty.IsNegative() {
		// A legitimate state, not a pricing error: corrections can put a
		// bucket's total below zero until new usage overtakes it.
		out.BelowZero = true
		return fmt.Sprintf("meter %q: net-negative usage so far (%s) after corrections — counted as 0", b.meter.EventName, qty.String()), out
	}
	// The quantity is what the operator needs to bill it by hand (#981).
	return fmt.Sprintf("meter %q: %s %s matched no price of the plan — not billed", b.meter.EventName, qty.String(), b.meter.Unit), out
}

// planMeter is one meter of one plan: the key of a closer's windows.
type planMeter struct {
	planID, meterID string
}

// sortedPlanMeters is the keys of windows in meter id order, then plan id,
// so a failure names the same meter on every run and every retry.
func sortedPlanMeters(sets ...map[planMeter][]usageInterval) []planMeter {
	seen := map[planMeter]bool{}
	var out []planMeter
	for _, set := range sets {
		for k := range set {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].meterID != out[j].meterID {
			return out[i].meterID < out[j].meterID
		}
		return out[i].planID < out[j].planID
	})
	return out
}

// rateCloseUsage prices a closer's usage: each (plan, meter) over its
// clamped windows, then, after a threshold invoice, over its full windows
// for the non-additive buckets that invoice deferred (ADR-066 §4), with one
// tier ladder continuing from that invoice. The period close and the cancel
// share it. Each priced bucket is a line in currency whose description ends
// with suffix; each bucket left out is logged under logPrefix and added to
// unpriced or belowZero.
func (e *Engine) rateCloseUsage(ctx context.Context, sub domain.Subscription, src PriceSource, reads *windowUsage, site rateSite, clamped, full map[planMeter][]usageInterval, wm thresholdWatermark, periodStart time.Time, currency, suffix, logPrefix string, unpriced unpricedUsage, belowZero netNegativeUsage) ([]domain.InvoiceLineItem, int64, error) {
	// Every (plan, meter) with a billable window. When a watermark exists, a
	// key whose clamped windows vanished (all its usage inside the threshold
	// invoice's window) can still owe a full-window bill for a deferred
	// non-additive bucket, so the full map's keys count too.
	var windows []rateWindow
	for _, k := range sortedPlanMeters(clamped, full) {
		for _, iv := range mergeUsageIntervals(clamped[k]) {
			windows = append(windows, rateWindow{planID: k.planID, meterID: k.meterID, iv: iv, pass: passClamped})
		}
		if wm.exists() {
			for _, iv := range mergeUsageIntervals(full[k]) {
				windows = append(windows, rateWindow{planID: k.planID, meterID: k.meterID, iv: iv, pass: passFull})
			}
		}
	}
	r := rater{prices: closeMeters{PriceSource: src, meters: reads.meters}, reads: reads, site: site, tenantID: sub.TenantID, customerID: sub.CustomerID}
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
// start. Each line takes its plan's currency. A failure names the window's
// meter.
func (e *Engine) ratePreviewUsage(ctx context.Context, sub domain.Subscription, src PriceSource, windows []rateWindow, periodStart time.Time, ladder *usageLadder, plans map[string]domain.Plan) ([]PreviewLine, []string, []leftOutBucket, error) {
	site := rateSite{preview: func(w rateWindow, err error) error {
		return fmt.Errorf("preview meter %q: %w", w.meterID, err)
	}}
	r := rater{prices: src, reads: claimReads{usage: e.usage, tenantID: sub.TenantID, customerID: sub.CustomerID}, site: site, tenantID: sub.TenantID, customerID: sub.CustomerID}
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
			lines = append(lines, b.previewLine(plans[b.w.planID].Currency))
			continue
		}
		warning, lo := b.previewLeftOut()
		warnings = append(warnings, warning)
		leftOut = append(leftOut, lo)
	}
	return lines, warnings, leftOut, nil
}
