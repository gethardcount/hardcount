# ADR-043: Drop `tax_rate_bp` immediately (no transition window)

**Status:** Accepted
**Date:** 2026-05-31
**Supersedes:** the "retain `tax_rate_bp` during transition" wording
in ADR-042. The `tax_rate_bp` storage is removed entirely.

## Summary

`tax_rate NUMERIC(7,4)` is the only tax-rate storage: migration 0105 dropped the `tax_rate_bp` basis-point columns at once instead of keeping both during a transition. With no deployments there was no staged rollout to protect, so a second column was only debt. The Settings tax rate takes a decimal percent (for example 8.875), and the manual provider computes in integer parts per million to keep four decimal places. Status: active.

## Context

ADR-042 added `tax_rate NUMERIC(7,4)` columns alongside the legacy
`tax_rate_bp bigint` and stipulated a standard "add new, populate,
switch readers, drop later" migration pattern. The dual-column state
is the textbook safe approach for **production systems with staged
rollouts** — protects against mid-deploy heterogeneity where some
replicas read the old column and others read the new.

Velox has zero such concern:

- Zero deployments. Single local DB, single binary.
- No staged rollout. No mid-deploy mix.
- The down migration in 0104 already drops the new column (losing
  precision data) — so the rollback safety the dual-column gives
  is already imperfect; we're paying the cost without getting the
  benefit.

Per `feedback_no_belt_and_suspenders` ("one path unless independent
failure modes warrant two") and `feedback_pre_launch_scoping` (zero
DPs means no "transition window" ever ends — "drop later" becomes
"after the next outage" the moment a paying customer exists), the
dual-column state is debt.

## Decision

**Drop `tax_rate_bp` immediately. `tax_rate NUMERIC(7,4)` is the
only rate storage.**

- Migration 0105 drops `tax_rate_bp` from `invoices`,
  `invoice_line_items`, `tenant_settings`.
- Domain types remove `TaxRateBP` fields entirely.
- `tax.Result.EffectiveRateBP`, `tax.ResultLine.TaxRateBP`,
  `tax.Breakdown.RateBP` — all removed. Only the `Rate` /
  `TaxRate` / `EffectiveRate` `float64` fields remain.
- `engine.TaxApplication.TaxRateBP` removed.
- Postgres store: ~10 SQL statements simplified (1 fewer column
  in INSERT/UPDATE/SELECT each).
- `subscription.ProrationTaxResult.TaxRateBP` removed.
- `creditnote.OriginalInvoiceInfo.TaxRateBP` → `TaxRate float64`.
- `hostedinvoice.InvoicePayload.TaxRateBP` → `TaxRate float64`.
- PDF render: bp display → decimal display (`fmt.Sprintf("%.4g%%",
  rate)`).
- TypeScript types: `tax_rate_bp: number` → `tax_rate: number`.
- Frontend Settings UI: input now binds directly to `form.tax_rate`
  in decimal percent; validator accepts `[0, 100]` (decimal) instead
  of `[0, 10000]` (bp).
- `ManualProvider` constructor takes `rate float64` (was `rateBP
  int64`). Internal math switches from bp-base (`× rateBP / 10000`)
  to ppm-base (`× ratePPM / 1_000_000`) — preserves 4-decimal
  precision in integer arithmetic without float drift. `ratePPM =
  int64(math.Round(rate * 10000))` computed once at construction.
- Validation message in `tenant.settings.go` updated from "bp 0..
  10000 (e.g. 1850 for 18.50%)" to "0..100 (e.g. 18.50 for 18.5%;
  up to 4 decimal places)".

## Consequences

- **One storage shape.** No more "which field is authoritative?"
  question. Every writer updates one column; every reader reads one
  column.
- **Smaller code.** ~50 lines net deletion across Go (struct
  fields, SQL placeholders, parameter slots), ~30 lines TS.
- **No precision regression.** The NUMERIC(7,4) column already had
  the precise value (populated by 0104's backfill from bp at the
  time + by every new write). 0105 simply removes the redundant
  bp column.
- **Operator UI now accepts decimal directly.** Settings → Tax rate
  input takes `8.875` instead of bp-as-integer. No more
  multiply-by-100 dance.
- **Existing rows preserved.** Migration 0104 populated `tax_rate`
  by `tax_rate_bp::numeric / 100`; that value is already authoritative.
  Dropping `tax_rate_bp` loses nothing.
- **Down migration is honest.** 0105's down migration re-creates
  `tax_rate_bp` and backfills from `tax_rate` (banker's rounded),
  matching the pre-0104 state lossy-but-recoverable shape.

## Pattern recorded

When you find yourself writing "keep the legacy column during
transition" in a pre-launch codebase, **don't**. The transition
window has no end at zero deployments — the legacy column becomes
permanent debt. Migrate-and-drop in one motion while there's no
staged-rollout cost to pay.

## Industry references

(Same as ADR-042 — Stripe Tax / Lago / Chargebee / Recurly all use
decimal-precision rate storage; none retain a bp parallel column.)
