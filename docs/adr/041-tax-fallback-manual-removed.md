# ADR-041: Remove `tax_on_failure=fallback_manual` (block-only)

**Status:** Accepted
**Date:** 2026-05-30
**Supersedes:** the `tax_on_failure=fallback_manual` setting introduced
by migration 0039 (`internal/platform/migrate/sql/0039_tax_block_retry.up.sql`).

## Summary

If tax calculation fails, the invoice stays a draft until the tax retry job ([ADR-017](017-tax-retry-and-auto-finalize.md)) or the operator resolves it; Velox never substitutes another rate. The `tax_on_failure=fallback_manual` option was removed, because it silently charged zero tax wherever no manual rate was set. Migration 0103 moved every tenant to `block`, the only value its CHECK constraint allows. Choosing `tax_provider=stripe_tax` without a Stripe client is an explicit error, and an operator who wants manual tax sets `tax_provider=manual`. Status: active.

## Context

Migration 0039 introduced `tenant_settings.tax_on_failure` with two
values:

- `block` — when Stripe Tax fails to compute tax (API outage, missing
  customer country, missing Stripe credentials, etc.), return the error
  to the engine. The engine defers the invoice to `tax_status=pending`;
  the TaxRetrier reconciler picks it up on the next scheduler tick.
- `fallback_manual` — when Stripe Tax fails, transparently substitute
  the result of the internal `ManualProvider` (which computes tax from
  the tenant's manual rate config for the customer's jurisdiction).

The default for new tenants was `block`. Migration 0039 line 46 set
every EXISTING tenant to `fallback_manual` to "preserve behavior at
migration time."

The 2026-05-30 design-debt audit identified this as residual debt:

1. **No operator has consciously chosen `fallback_manual`.** Every
   existing tenant was opted in by migration 0039, not by user action.
2. **The fallback's edge-case behavior is wrong.** `ManualProvider`
   returns ZERO when no manual rate is configured for the customer's
   jurisdiction. Operators who pick `fallback_manual` intending "fall
   back to my manual rate" silently get "fall back to zero tax" in
   every jurisdiction they didn't pre-configure rates for. The fallback
   produces a different answer than the operator intended, with no
   surface-level signal beyond a `slog.Warn` line.
3. **The TaxRetrier already handles transient Stripe failures.** The
   reconciler exists specifically to retry `tax_status=pending` invoices
   when Stripe Tax becomes reachable again. The "block" path uses it
   correctly; `fallback_manual` competes with the retrier by shipping
   the invoice with an approximate (often zero) tax NOW, forcing the
   operator to issue credit notes later to reconcile the difference.
   That's strictly worse operational hygiene than letting the retrier
   resolve transient outages.
4. **Per `feedback_no_silent_fallbacks`**: engine paths that can't
   produce a correct answer must fail loudly, not substitute a default.
   Logging at `Warn` doesn't make a fallback not-silent — the customer
   still gets a wrong-tax invoice + no actionable signal at calc time.
5. **Per `feedback_no_belt_and_suspenders`**: the fallback's failure
   modes (manual rate missing for jurisdiction → returns zero) are not
   independent of the failure mode it's meant to rescue from (Stripe
   API down). The retrier IS the recovery path; `fallback_manual` is a
   parallel mechanism with worse outcomes.

## Decision

**Cut `fallback_manual`. Block-only post-migration.**

- Migration 0103 sets every tenant currently at `fallback_manual` to
  `block` and tightens the CHECK constraint to allow only `block`.
- `internal/tax/stripe.go` `handleFailure` always returns the error;
  the `p.fallback *ManualProvider` field and the constructor's
  `fallback` parameter are removed.
- `internal/tax/resolver.go` no longer constructs a manual fallback
  when `tax_provider=stripe_tax` is selected without a wired Stripe
  client — returns an explicit error: *"tax_provider=stripe_tax but
  no Stripe client wired — set tax_provider=manual or configure
  Stripe"*. Operators who want manual billing must explicitly select
  `tax_provider=manual` at the tenant level.
- `internal/tax/provider.go` removes the `OnFailureFallbackManual`
  constant. `OnFailureBlock` is the only valid `OnFailure` value.
  `Request.OnFailure` is retained for forward-compat (future shapes
  like `defer_with_delay` can reuse the field).
- `internal/tenant/settings.go` validation accepts only `block` (was
  `block` or `fallback_manual`).
- `internal/domain/audit.go` `TenantSettings.TaxOnFailure` comment
  block is updated to reflect the post-ADR-041 contract.
- `internal/tax/providers_test.go` `stripe_tax_without_clients_falls_back_to_manual`
  test case rewritten to assert the new error-loud behavior.
- Prometheus metric `velox_tax_outcome_total` no longer emits the
  `outcome="fallback"` label value (only `deferred` remains for the
  failure-mode signal).

## Consequences

- **Operators get correct tax or no invoice.** Stripe Tax outage →
  invoice goes to `tax_status=pending` + Attention banner + TaxRetrier
  takes over. No silent shipped-wrong-tax invoices.
- **Operators who genuinely want manual billing must say so explicitly.**
  `tax_provider=manual` is the path. Mixed Stripe+manual ("Stripe
  preferred, manual fallback") is no longer expressible — the use case
  was never named by any DP and the failure mode was wrong (silent
  zero, not the configured manual rate).
- **One less branch in the failure path.** `handleFailure` collapses
  to a single defer + metric increment.
- **No code in production was using `fallback_manual` consciously.**
  Velox is pre-launch (zero customers). Migration 0039 had set every
  existing tenant including the test tenant; no operator chose this.
- **Schema retained.** The `tax_on_failure` column stays (forward-compat
  for future failure-handling shapes). Only the CHECK constraint and
  the runtime branch are removed.

## Revisit trigger

Resurrect `fallback_manual` (or a runtime equivalent) when:
- A signed design partner names a use case where they specifically
  want "Stripe preferred, MY configured manual rate as fallback for
  jurisdictional gaps" (NOT zero-fallback — the silent-zero edge case
  is the actual bug; if rebuilt, the fallback must hard-error when no
  manual rate matches). The rebuild would add a new policy value
  (e.g. `fallback_to_configured_manual_strict`) with the no-rate-match
  → block guard baked in.
- AI-native peer convergence shifts in a way that makes mixed-provider
  tax composition the industry-standard pattern.

Until then, block-only is the contract.

## Industry references

- Stripe Billing: when Stripe Tax fails, the invoice goes to draft
  state with a tax error; Stripe doesn't substitute a default.
- Lago: defers to operator-configured tax (no auto-fallback).
- Chargebee + Avalara: blocks the invoice on tax failure.
- The "fallback to approximate, reconcile later" pattern doesn't
  appear in the AI-native or traditional SaaS billing peer set.

## Related cuts

- ADR-040: outbox env flags removed. Same shape — boot-time flag /
  setting whose "off" branch silently weakens correctness without an
  operator-visible signal.
- ADR-039: coupons removed. Same shape — feature kept on parity
  reasoning without a named DP use case.
