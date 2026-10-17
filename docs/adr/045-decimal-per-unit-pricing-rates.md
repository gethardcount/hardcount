# ADR-045: Decimal per-unit pricing rates + multi-dim cycle billing

**Status:** Accepted
**Date:** 2026-06-01
**Relates to:** ADR-044 (canonical AI token metering model). Follows the
precedent of ADR-042 (tax rate `bigint` bp → `NUMERIC`).
**Supersedes:** the `int64`-cents per-unit rate fields introduced in
migration 0001, and the single-rule-only usage-billing path in
`billing.billOnePeriod`.

## Summary

Per-unit price rates (`flat_amount_cents`, graduated-tier `unit_amount_cents`, `overage_unit_amount_cents`, and the same fields on customer price overrides) are arbitrary-precision decimals. They are stored as `NUMERIC` and sent as JSON strings such as `"0.0003"`, so sub-cent prices such as $3 per million tokens can be expressed. Fixed fees and every invoice amount stay whole `int64` cents, and the charge is rounded to a whole cent once. The cycle close also prices multi-dimension meters through their pricing rules, the same path the preview uses. Status: active; showing a sub-cent unit price on invoice lines came later ([ADR-054](054-effective-unit-price-decimal-display.md)).

## Context

ADR-044 made the AI wedge a one-meter / N-rule model: a `tokens` meter
priced per `{model, token_type}`. Wiring that end-to-end (the headline
demo: point a LiteLLM proxy at Velox and the AI plan bills automatically)
surfaced two defects that meant the wedge **could not actually bill**.

### Gap 1 — per-unit rates were integer cents, so sub-cent prices were inexpressible

Velox stored every per-unit rate as `int64` cents and `flat` mode prices
`quantity × flat_amount_cents`. AI token pricing is sub-cent-per-unit:
$3.00 per **1,000,000** tokens = 0.0003 cents/token. Integer cents cannot
represent that. The shipped `anthropic_style` recipe authored
`flat_amount_cents: 300` and *commented* it "$3.00 per million" — but in
per-unit `flat` mode that bills **$3.00 per token**, a 1,000,000× overcharge.

Peers price sub-cent usage with **decimal unit prices**, not integers:

| Platform | Per-unit rate format |
|---|---|
| Stripe | `unit_amount_decimal` — decimal string, up to 12 decimal places (verified) |
| Lago (open source) | `BigDecimal` amounts (verified) |
| Orb / Metronome | usage-based AI billers (tokens/compute); decimal-precision internals are sales-gated / not publicly documented |
| Velox (pre-ADR-045) | `int64` cents — whole-cent floor |

Stripe is the load-bearing anchor and the closest fit: its docs state
`unit_amount_decimal` is for sub-cent usage rates (e.g. "0.05 cents per MB")
**and that the customer is still charged an integer minor-unit amount** — the
exact "decimal rate, integer-cent line total" split this ADR adopts.

### Gap 2 — the cycle billing path ignored multi-dim meters entirely

`billOnePeriod` priced usage by reading each meter's single linked
`rating_rule_version_id` (`AggregateForBillingPeriodByAgg` → one rate ×
one summed quantity). A multi-dim meter (the AI `tokens` meter) has **no**
single linked rule — it is priced by `MeterPricingRule` rows resolved via
`AggregateByPricingRules`. So `billOnePeriod` **skipped** the meter and
emitted a `$0` invoice ("no billable lines"). Only `preview` and the
threshold scan used the pricing-rules path — `preview.go` even *claimed*
"the cycle scan calls the same path," which was untrue (doc-code drift).

## Decision

1. **Per-unit rate fields become arbitrary-precision decimals.**
   `RatingTier.UnitAmountCents`, `RatingRuleVersion.FlatAmountCents`, and
   `RatingRuleVersion.OverageUnitAmountCents` (and their mirrors on
   `CustomerPriceOverride`, `RecipeRatingRule`, and the create-inputs)
   change from `int64` to `shopspring/decimal.Decimal`. DB columns
   `flat_amount_cents` / `overage_unit_amount_cents` on
   `rating_rule_versions` and `customer_price_overrides` go `BIGINT →
   NUMERIC` (migration 0108); `graduated_tiers` is JSONB and needs no DDL.
   JSON wire form is a **string** (`"0.0003"`) — shopspring/decimal's
   native marshaling, matching how Velox already serializes decimal
   quantities (`usage_gte`, usage quantities).

2. **Fixed fees stay `int64` cents.** `base_amount_cents`,
   `package_amount_cents`, and — critically — **every invoice line amount
   and total** remain whole `int64` cents. Only the per-unit RATE
   (multiplied by a decimal quantity) gains precision; `ComputeAmountCents`
   still rounds the final amount to whole cents (banker's rounding). You
   cannot charge a customer a fractional cent.

3. **`billOnePeriod` prices multi-dim meters via `AggregateByPricingRules`.**
   When a meter has `MeterPricingRule` rows it emits one invoice line per
   claimed rule bucket, using the same pricing-rule resolution + per-bucket
   pricing + line description (`usageLineDescription`) as `previewMeter` — so
   rate buckets and line text match the preview. Meters with no pricing rules
   keep the legacy single-rule path. Cap-scaling is applied to the rule
   buckets directly (they are raw, pre-cap quantities, unlike the pre-scaled
   `usageTotals`). Note: `preview` is a full-period *estimate* and (as before
   this change) does not replicate usage-cap scaling or mid-period segment
   proration — for a sub with neither (the common case, incl. the AI recipes)
   preview equals the invoice; closing that gap for capped/segmented subs is a
   separate, pre-existing follow-up.

4. **Recipes are re-authored per-token.** `anthropic_style` and
   `openai_style` express each rate as cents-per-token decimal
   (published $/1M ÷ 1,000,000); decimal precision also lets the derived
   cache multiples (0.1× / 1.25× / 2×) be exact instead of cent-rounded.
   `replicate_style` (per-second whole-cent rates) is unchanged.

## Consequences

- The wedge bills end-to-end: a realistic LiteLLM payload
  (`claude-3-5-sonnet-20241022`, prompt-cache hit) rates to an exact
  invoice. Guarded by `TestLiteLLM_WedgeE2E` (1,830¢ for 1M input + 1M
  cache_read + 1M output) — the CI test that would have caught both gaps.
- **Display artifact:** an invoice line's `unit_amount_cents` (still
  `int64`) rounds to `0` for sub-cent rates (e.g. 1M tokens @ 0.0003¢ →
  line amount 300¢, unit shown 0¢). The line `amount_cents` is exact;
  surfacing a decimal per-unit on the line is a deferred display-only
  follow-up (invoice line amounts intentionally stay integer cents).
- Migration 0108 down (`NUMERIC → BIGINT`) is lossy for any fractional
  rate (rounds to whole cent) — inherent to reverting a precision upgrade;
  acceptable pre-launch.
- API contract change: `flat_amount_cents` / graduated-tier
  `unit_amount_cents` on rating rules are now JSON **strings**. Pre-launch,
  the only consumer is web-v2 (updated in the same change).
