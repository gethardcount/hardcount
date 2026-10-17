# ADR-042: Tax-rate decimal precision + proration integer day-ratio

**Status:** Accepted (partly superseded by [ADR-043](043-drop-tax-rate-bp.md) on 2026-05-31: the `tax_rate_bp` transition)
**Date:** 2026-05-31
**Supersedes:** the `tax_rate_bp bigint` storage shape (introduced
migration 0001, widened in 0014) AND the `prorationFactor float64`
math in `internal/subscription/handler.go`.

## Summary

Tax rates are stored as `tax_rate NUMERIC(7,4)`, four decimal places as Stripe Tax returns them, so rates such as 8.875% are kept exactly. Stripe's `percentage_decimal` string is parsed without float truncation. Mid-period proration uses whole days, `money.RoundHalfToEven(amount × remaining_days, total_days)`, instead of a float factor, so the result is exact and the same on every machine. The plan to keep the old `tax_rate_bp` column during a transition was dropped by [ADR-043](043-drop-tax-rate-bp.md), which removed the column at once. Status: active, partly superseded by ADR-043.

## Context

The 2026-05-31 financial-precision audit (research-backed against
Stripe Tax / Lago / Chargebee / Recurly schemas) found two industry-
standard gaps in Velox's money-handling code.

### Gap 1 — Tax rate stored as integer basis points (1 bp = 0.01%)

Velox stored every tax rate as `bigint` basis points. Peers all use
higher precision:

| Platform | Format | Precision |
|---|---|---|
| Stripe Tax | Decimal string | 4 decimal places (`percentage_decimal: "8.8750"`) |
| Stripe Billing | Decimal | 4 places |
| Lago | BigDecimal | Unlimited |
| Chargebee | Numeric | Up to 20 places |
| Recurly | Float | 3 places |
| Velox (pre-ADR-042) | Integer bp | 2 places (0.01%) |

Real-world rates that lose precision under bp:

- **NYC 8.875%** (state 4.0 + city 4.5 + MCTD 0.375) → 888 bp = 8.88%, losing 0.005%
- **Quebec QST 9.975%** → 997 bp = 9.97%, losing 0.005%
- **Hawaii GET 4.7120%** → 471 bp = 4.71%, losing 0.0020%
- **Tennessee sub-rates** at 0.25% precision → unrepresentable

The actual TAX AMOUNT (`tax_amount_cents`) is computed by the
provider (Stripe Tax) at full precision and stored verbatim — so
the customer is always billed correctly. The rate field is used for
DISPLAY and AUDIT. A 1¢ display discrepancy compounds into an
auditor's "8.88% × $99 = $8.79? but $8.79 / $99 = 8.879…" cross-
check failure.

Additionally, Stripe Tax explicitly returns `percentage_decimal` as
a STRING precisely to avoid lossy float round-trip. Velox's
ingestion did `strconv.ParseFloat(pct, 64)` → `int64(v * 100)`,
truncating toward zero (losing 0.005% on 8.875% — stored 887 instead
of banker's-rounded 888). The integer bp storage compounded the
truncation: even after parsing precisely, storage would lose
precision.

### Gap 2 — Proration computed via `float64` factor

`internal/subscription/handler.go` used `prorationFactor float64`
(remaining hours / total hours, divided by 24) at every proration
site. The actual math was `diff := float64(newAmount-oldAmount) *
prorationFactor; proratedCents := int64(math.RoundToEven(diff))`.

This introduced ULP error on large amounts (~$36M+ visible) and was
non-deterministic across architectures. Industry peers (Stripe
Billing, Lago, Orb) all use integer day-ratio math:
`amount × remaining_days / total_days` with banker's rounding.

Velox's engine path (`internal/billing/engine.go:655`) already used
this pattern via `money.RoundHalfToEven`. The subscription handler
path had drifted.

## Decision

### Tax rate (gap 1)

- Migration 0104 adds `tax_rate NUMERIC(7,4)` columns to `invoices`,
  `invoice_line_items`, `tenant_settings`, backfilled from
  `tax_rate_bp / 100`. The `tax_rate_bp` columns are retained for
  backward compat during transition; a future migration drops them
  once all readers are confirmed switched.
- `NUMERIC(7,4)` chosen to match Stripe Tax's 4-decimal precision
  (7 digits, 4 after decimal → max 999.9999%, sufficient for every
  real tax rate including compound).
- Go domain types add `TaxRate float64` fields alongside `TaxRateBP`
  (kept for backward compat). `domain.Invoice`,
  `domain.InvoiceLineItem`, `domain.InvoiceDiscountUpdate`,
  `domain.InvoiceTaxRetryUpdate` all updated.
- `tax.Result.EffectiveRate` and `tax.ResultLine.TaxRate` carry the
  precise float from provider.
- New `internal/tax/stripe.go:parseStripeRate` helper parses the
  `percentage_decimal` string into both the precise float (for the
  new column) AND the banker's-rounded bp (for the legacy column).
  Replaces the pre-existing `int64(v * 100)` truncation at two call
  sites.
- `ManualProvider` populates `TaxRate` from `rateBP / 100` —
  manual-entered rates remain at bp precision (operator UI accepts
  bp; precision win is only on Stripe ingestion until the operator
  UI is updated in a follow-up).
- Engine threads `TaxRate` through `TaxApplication` → invoice rows
  and line items.
- Postgres store reads + writes both `tax_rate_bp` and `tax_rate`
  on all INSERT/UPDATE statements. `tenant_settings` derives
  `tax_rate = tax_rate_bp::numeric / 100` at the SQL boundary
  (operator UI unchanged; precision win on the Stripe ingestion
  path is the headline).
- `velox_tax_outcome_total` Prometheus metric and the existing
  audit-row tax fields are unchanged.

### Proration (gap 2)

- `remainingPeriodFactor` (returning `float64`) → `remainingPeriodRatio`
  (returning `remainingDays, totalDays int64`).
- `itemProrationSpec.prorationFactor float64` →
  `remainingDays, totalDays int64`. All struct literal sites
  updated.
- `atomicAddItemWithProration`, `atomicUpdateItemWithProration`,
  `atomicRemoveItemWithProration` signatures take the two int64s
  instead of the float.
- The actual cents math at the proration site changes from
  `int64(math.RoundToEven((newAmount-oldAmount) * spec.prorationFactor))`
  to
  `money.RoundHalfToEven((newAmount-oldAmount) * spec.remainingDays, spec.totalDays)`.
- `ProrationDetail.ProrationFactor` (public event payload) is
  preserved as `float64` derived from `remainingDays/totalDays`
  for downstream consumers — display-only.

## Consequences

- **Precision on Stripe ingestion:** NYC 8.875%, Quebec 9.975%,
  Hawaii 4.7120% all round-trip exactly through Velox's storage
  and display layer.
- **Audit cross-checks:** rate × subtotal = tax_amount holds at
  the 4-decimal precision level for Stripe-provided rates.
- **Proration math:** deterministic across architectures; no more
  float ULP drift on large amounts.
- **Backward compat:** `tax_rate_bp` columns stay populated (writes
  set both; reads can still consume the bp column). Existing dashboard
  /PDF rendering continues to work without UI changes.
- **Operator UI:** unchanged in this ADR. The Settings → Tax rate
  input still accepts basis points (operator enters whole-bp values
  only). A follow-up will add a decimal-input UI that accepts
  `8.875` directly and persists via the new column. Estimated
  ~30 LOC for the UI; not blocking on this migration.
- **Migration safety:** the `tax_rate` columns are populated on
  apply via UPDATE-from-bp; no manual data fix needed. Down migration
  drops the new columns; existing readers fall back to bp.
- **Metric:** `velox_tax_outcome_total` already drops the legacy
  `outcome=fallback` label per ADR-041; this ADR adds no new metric.

## Industry references

- Stripe Tax Rate API:
  https://docs.stripe.com/api/tax_rates
- Stripe Tax Calculations:
  https://docs.stripe.com/api/tax/calculations/object
- Lago BigDecimal usage:
  https://github.com/getlago/lago/wiki/%5BRuby%5D-On-using-BigDecimal
- Chargebee multi-decimal support:
  https://www.chargebee.com/docs/2.0/multi-decimal-support.html
- Recurly TaxInfo:
  https://recurly.github.io/recurly-client-ruby/Recurly/Resources/TaxInfo.html

## Follow-ups (not in this ADR)

- B7.1 / B7.2 currency precision table + ISO 4217 full coverage
  (latent 100× JPY bug). Tracked separately in the B7 roadmap.
- B7.5 cosmetic polish — replace `float64(.../100)` display strings
  in credit / credit-note error messages with `money.Format`.
- Decimal-input UI on Settings → Tax rate (accept `8.875` directly
  instead of basis points).
- Eventually drop the `tax_rate_bp` columns and rename `tax_rate`
  back if desired. Schedule TBD.
