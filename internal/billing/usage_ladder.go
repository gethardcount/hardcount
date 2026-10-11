package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// usageLadder prices a closer's usage slices so a bucket's tiers continue
// from what a mid-period threshold fire already billed, instead of restarting
// at tier 0 (P21, ADR-066 amendment 2026-10-11). Without a fire, or for a
// non-additive bucket, price is ComputeAmountCents(rule, qty): unchanged.
//
// It tracks the amount billed as well as the quantity, so a slice bills
// F(quantity so far + q) − amount billed so far, floored at 0. For flat and
// graduated rules that is exact (fire + close = F(total)). A non-monotone
// package rule can floor a slice at 0, and a later slice of the same bucket
// then recovers the difference.
type usageLadder struct {
	wm  thresholdWatermark
	qty map[string]decimal.Decimal // bucket key → quantity priced so far this period
	amt map[string]int64           // bucket key → amount billed so far this period
}

func newUsageLadder(wm thresholdWatermark) *usageLadder {
	return &usageLadder{wm: wm, qty: map[string]decimal.Decimal{}, amt: map[string]int64{}}
}

// price returns the amount for qty more units of the bucket under rule.
// bucketKey identifies the closer's bucket (meter + pricing rule); rule is
// the RESOLVED rule, whose id is how the fire's line is found.
func (l *usageLadder) price(rule domain.RatingRuleVersion, meterID, bucketKey string, mode domain.AggregationMode, qty decimal.Decimal) (int64, error) {
	if !l.wm.exists() || nonAdditiveMode(mode) {
		return domain.ComputeAmountCents(rule, qty)
	}
	if _, seen := l.qty[bucketKey]; !seen {
		q, a, err := l.wm.billedUsage(meterID, rule.ID)
		if err != nil {
			return 0, err
		}
		l.qty[bucketKey], l.amt[bucketKey] = q, a
	}
	cumulative := l.qty[bucketKey].Add(qty)
	total, err := domain.ComputeAmountCents(rule, cumulative)
	if err != nil {
		return 0, err
	}
	amount := max(total-l.amt[bucketKey], 0)
	l.qty[bucketKey] = cumulative
	l.amt[bucketKey] += amount
	return amount, nil
}

// billedUsage is the quantity and amount the fire billed for one (meter,
// resolved rating rule version) — zero when the fire has no such line. Two
// matching lines mean two buckets the close cannot tell apart, so it errors.
func (w thresholdWatermark) billedUsage(meterID, ratingRuleVersionID string) (decimal.Decimal, int64, error) {
	var (
		qty   decimal.Decimal
		amt   int64
		found bool
	)
	for _, li := range w.lines {
		if li.LineType != domain.LineTypeUsage || li.MeterID != meterID || li.RatingRuleVersionID != ratingRuleVersionID {
			continue
		}
		if found {
			return decimal.Zero, 0, fmt.Errorf("threshold invoice has two usage lines for meter %s rating rule %s — cannot tell which bucket each belongs to, so the close cannot continue its tiers; give each pricing rule its own rating rule", meterID, ratingRuleVersionID)
		}
		found = true
		qty = li.QuantityDecimal
		if qty.IsZero() {
			qty = decimal.NewFromInt(li.Quantity) // lines written before quantity_decimal
		}
		amt = li.AmountCents
	}
	return qty, amt, nil
}

// checkLadderSeeds fails loudly when two buckets of one meter resolve to the
// same rating rule version and the fire billed that version: the fire's line
// carries no pricing-rule id, so its quantity could seed either bucket's
// ladder. It reads the meter's configuration (default binding plus every
// pricing rule), not which buckets have usage, because a bucket with no
// post-fire usage would otherwise silently hand its seed to the other.
// A no-op without a fire.
func (e *Engine) checkLadderSeeds(ctx context.Context, sub domain.Subscription, meterID string, pricingRules []domain.MeterPricingRule, periodStart time.Time, wm thresholdWatermark) error {
	if !wm.exists() || len(pricingRules) == 0 {
		return nil
	}
	meter, err := e.pricing.GetMeter(ctx, sub.TenantID, meterID)
	if err != nil {
		return fmt.Errorf("get meter %s: %w", meterID, err)
	}
	pinned := make([]string, 0, len(pricingRules)+1)
	if meter.RatingRuleVersionID != "" {
		pinned = append(pinned, meter.RatingRuleVersionID)
	}
	for _, r := range pricingRules {
		pinned = append(pinned, r.RatingRuleVersionID)
	}
	seen := map[string]int{}
	for _, id := range pinned {
		rule, err := e.resolveRatedRule(ctx, sub.TenantID, sub.CustomerID, id, periodStart)
		if err != nil {
			return fmt.Errorf("resolve pricing for meter %s rule %s: %w", meterID, id, err)
		}
		seen[rule.ID]++
		if seen[rule.ID] == 2 {
			q, _, err := wm.billedUsage(meterID, rule.ID)
			if err != nil {
				return err
			}
			if !q.IsZero() {
				return fmt.Errorf("meter %s: two of its pricing buckets resolve to rating rule %s, which the threshold invoice billed — the close cannot tell whose tiers to continue; give each pricing rule its own rating rule", meterID, rule.ID)
			}
		}
	}
	return nil
}
