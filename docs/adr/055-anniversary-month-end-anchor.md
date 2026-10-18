# ADR-055: Anniversary billing clamps to month-end from a persisted anchor day

**Date:** 2026-06-18
**Status:** Accepted (supersedes the ADR-058 §"Explicitly NOT a bug" note on anniversary month-end overflow)

## Summary

Anniversary and yearly billing keep the operator's day of month, clamped to each month's last day: an anchor on the 31st bills Jan 31, Feb 28, Mar 31, Apr 30. The day is stored in `subscriptions.billing_anchor_day` (migration 0120), and every advance starts from it, so a short month no longer shifts later periods. The old code advanced from the previous boundary and drifted for good (Jan 31, Mar 3, Apr 3). A cross-interval swap or a threshold reset recomputes the anchor day, and calendar billing (always the 1st) is unchanged. Status: active; this replaces the "not a bug" note in [ADR-058](058-billing-date-math-tenant-timezone.md).

## Context

An adversarial e2e audit of the billing date-math (2026-06-18) found a real, high-severity correctness bug in **anniversary** monthly/yearly billing for anchor days **29/30/31**.

The cycle-close advance was **self-referential**: it computed the next boundary as `periodEnd + 1 interval` off the *previously computed* boundary, using Go's `time.AddDate`, which normalizes month overflow forward (`Jan 31 + 1mo` → "Feb 31" → **Mar 3**). Because each cycle advanced off the already-drifted value, the billing day **ratcheted and never recovered**:

```
Anchor Jan 31, anniversary monthly — BEFORE this fix:
  Jan 31 → Mar 3 → Apr 3 → May 3 → Jun 3 …   (locked on the 3rd forever)
```

The same hit day-30 (→ the 2nd) and day-29 (→ the 1st) after the first February, and a leap Feb-29 yearly anchor ratcheted to Mar 1. February's calendar *time* stayed covered (inside the stretched first period), but: (a) the operator's chosen billing day was permanently lost, (b) no invoice was dated in February, and (c) the proration denominator (`fullBillingCycleDays`, derived from the same advance) was contaminated for those subs, mischarging mid-cycle upgrades/downgrades.

**Industry standard (verified, live docs):**
- **Stripe** — *"A monthly subscription with a billing cycle anchor date of January 31 bills the last day of the month closest to the anchor date, so February 28 (or February 29 in a leap year), then March 31, April 30, and so on."* ([docs.stripe.com/billing/subscriptions/billing-cycle](https://docs.stripe.com/billing/subscriptions/billing-cycle))
- **Chargebee / Lago** — clamp a high day-of-month to the month's last day and snap back when the month allows.

ADR-058 had explicitly declared the `Jan 31 → Mar 3` overflow "not a bug, do not fix it." That was wrong: it conflated the (correctly-fixed) timezone-offset issue with the month-end clamp, a separate, real gap. This ADR overturns that note.

## Decision

Bill anniversary subscriptions on the **operator's intended day-of-month, clamped to each target month's last day**, advancing from a **persisted anchor** rather than the drifted boundary.

```
Anchor Jan 31, anniversary monthly — AFTER this fix (verified by test):
  Jan 31 → Feb 28 → Mar 31 → Apr 30 → May 31 → Jun 30 → Jul 31 …
```

1. **Persist the anchor day.** New `subscriptions.billing_anchor_day SMALLINT` (migration 0120), the day-of-month (1..31) the operator's billing falls on, captured at activation in the tenant timezone (`domain.AnchorDayFor`). It is **0 for calendar-monthly** subs (their boundary is always the 1st, so the anchor is meaningless) and **0 for legacy/unset rows** — in which case the advance falls back to the historical `addIntervalIn` path, so the column is purely additive with no behavior change when unset.
2. **Clamp on advance.** `domain.NextBillingPeriodEnd` and `domain.AddBillingInterval` take the anchor day and, for yearly + anniversary-monthly, place `day = min(anchorDay, lastDayOf(targetMonth))` (`advanceAnchored`). Because the day comes from the *stored* anchor — not the (possibly already-clamped) previous boundary — `min` restores the higher day in long months. Calendar-monthly is unchanged (snap to the 1st; anchor ignored).
3. **Recompute on re-anchor.** The two paths that re-anchor the cycle to `now` — cross-interval plan swap and threshold `ResetBillingCycle` — recompute `billing_anchor_day` for the new cadence (and the threshold-reset path now routes through `NextBillingPeriodEnd`, not the interval-only advance, so a calendar sub re-snaps to the 1st — a secondary fix from the same audit).

Self-referential advance is the root cause, so the fix **must** persist the anchor; a clamp computed off the drifted boundary cannot recover the original day (it's already lost). This is the minimal-schema form of Stripe's `billing_cycle_anchor` (the day component, which is what month-end clamping needs).

## Alternatives considered

- **Clamp without persisting** (off `periodEnd`'s day). Rejected: once `Jan 31` has drifted to `Mar 3`, the day `3` is all that remains — no clamp can restore `31`. The anchor must be stored.
- **Store the full anchor instant** (Stripe `billing_cycle_anchor` parity). Deferred: the day-of-month is sufficient for month-end correctness, and `periodEnd` already carries the time-of-day; storing the full instant would change the advance model (compute `anchor + N` intervals) across every call site for no added correctness here.
- **Backfill existing rows precisely in the tenant TZ.** The migration backfills from `current_billing_period_start`'s day-of-month (best-effort, pre-launch / local-only); new subs set it precisely at activation. Calendar subs land on day 1, a clamp no-op.

## Consequences

### Positive
- Anniversary subs (and yearly) bill the operator's chosen day, clamped to month-end and restored in long months — Stripe/Chargebee/Lago parity. Leap Feb-29 anchors bill Feb 28 in common years, Feb 29 in leap years.
- The proration denominator for month-end subs is de-contaminated (it now reflects the true cycle length).
- Additive column; `0` preserves the exact legacy path, so calendar subs and unset rows are unaffected.

### Risks / open items
- The cross-interval-swap and threshold-reset re-anchor recompute the anchor in a separate write from the period update (existing non-atomic pattern); fails safe (a stale anchor day only affects the next cycle's day, recoverable).
- `exempt`/multi-item subs share one anchor (first item's interval), consistent with the existing single-cadence constraint.

## References
- ADR-058 (tenant-TZ date-math; this supersedes its anniversary-month-end "not a bug" note), ADR-042 (full-cycle proration denominator).
- Memory: `feedback_verify_examples_before_stating`, `feedback_billing_accuracy`, `feedback_verify_stripe_parity_claims`.
- Guarded by `TestAnniversaryMonthEnd_ClampsAndRestores`, `TestYearlyLeapAnchor_ClampsAndRestores`, `TestAnniversaryAnchorDayZero_LegacyFallback`, `TestAnchorDayFor`.
- [Stripe billing cycle](https://docs.stripe.com/billing/subscriptions/billing-cycle), [Chargebee](https://www.chargebee.com/docs/2.0/billing-cycle.html).
