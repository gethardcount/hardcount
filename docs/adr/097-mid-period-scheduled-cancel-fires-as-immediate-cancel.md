# ADR-097: A due mid-period `cancel_at` fires as an immediate cancel at that instant

**Status**: Accepted 2026-07-18 (amended by [ADR-118](118-cancel-and-swap-relief-in-the-change-transaction.md) on 2026-10-12 and [ADR-120](120-invoice-grace-period-for-late-usage.md) on 2026-10-14)
**Context**: FLOW TC8 walkthrough live-find; fixes the stranded-cancel money bug.

## Summary

A scheduled cancel whose `cancel_at` falls inside a billing period fires as an immediate cancel at that instant. Before this, the billing scans looked only at period ends, so such a subscription renewed past its own cancel date. One transaction cancels it, bills usage and any in-arrears base fee up to `cancel_at`, and writes relief for the unused prepaid part, never as a cash refund. `canceled_at` and the final invoice's period end are the `cancel_at` instant, even when the fire runs late. Status: active, amended: all relief is written in the cancel transaction ([ADR-118](118-cancel-and-swap-relief-in-the-change-transaction.md)), and wall-clock runs fire after the invoice grace period ([ADR-120](120-invoice-grace-period-for-late-usage.md)).

## The bug

`subscriptions.cancel_at` strictly between billing boundaries never fired.
All three due-subscription scans matched only `next_billing_at <= now`, and
`billOnePeriod` binds every decision to the period boundary
(`now = *sub.NextBillingAt`), so `shouldFireScheduledCancel` never saw a
mid-period timestamp. Observed live: a yearly sub with
`cancel_at = period_end + 15d` renewed straight past its own cancellation
date; it would have billed the full second year (~$319) at the next
boundary — 11.5 months after the customer canceled — then canceled with the
wrong `canceled_at`. The only prior test pinned `cancel_at == periodEnd`.

How such a state arises through the API: `ScheduleCancel` requires
`cancel_at >= current_billing_period_end` (mid-*current*-period cancels are
rejected 422 — proration at schedule time is out of scope). But any
timestamp past the current boundary is accepted, and the moment the cycle
renews past the boundary, a once-"future" `cancel_at` is now strictly
inside the *new* current period. Boundary drift via plan swaps or trial
extensions can produce the same shape. The schedule-time guard is kept
as-is; this ADR fixes the *firing* side.

## Decision

1. **Scan pickup.** The three due-subscription queries (wall cron, tenant
   run, clock catchup) gain a second arm:
   `OR (s.status = 'active' AND s.cancel_at IS NOT NULL AND s.cancel_at <= <now|tc.frozen_time>)`.
   Gated to `active` because `FireScheduledCancellation`'s CAS only accepts
   active subs and trialing cancels belong to the trial scan (ADR-069).

2. **Execution = the immediate-cancel machinery, at `cancel_at`.** When the
   billing loop meets a sub whose `cancel_at` is due strictly before
   `next_billing_at`, it does NOT invent a new billing mode. It executes the
   same atomic composition the operator immediate-cancel uses
   (`CancelAtomicWithBill`): one tx that CAS-flips the sub
   (`canceled_at = cancel_at`, schedule fields cleared,
   `subscription.canceled` webhook enqueued with `canceled_by="schedule"`),
   then — inside the same tx — bills the final partial period
   (`BillFinalOnImmediateCancelTx`: usage to `cancel_at` + prorated
   in_arrears base) and writes prepaid-relief credit-note drafts
   (`BillOnCancelDraftsTx`: unused prepaid in_advance portion returns as
   balance credit, never cash — the shipped six-platform-verified policy).
   Post-commit: finalize the final invoice, issue the drafts (or the
   unpaid-source `BillOnCancel` fallback) — identical to the operator path.

   The engine reaches this through a narrow consumer-defined executor
   interface implemented by `subscription.Service` (the coordinator wires
   it), keeping the domain boundary rules intact: the engine detects
   due-ness; the subscription domain owns the transition.

3. **Time binding — split, mirroring the immediate path.** Only the
   *contractual* fields backdate to `cancel_at`: `canceled_at`, the final
   invoice's `billing_period_end`, the relief window, the webhook payload.
   Everything *operational* anchors at effective-now (clock frozen-time /
   wall now): invoice `IssuedAt`/`DueAt`, `updated_at`. This is the exact
   split `billFinalOnImmediateCancelImpl` already uses (period_end =
   canceled_at, IssuedAt/DueAt = now). It matters for the remediation
   cohort — subs already months past their `cancel_at` at deploy: anchoring
   DueAt at `cancel_at` would mint invoices born overdue and their first
   customer contact would be a dunning notice.

4. **No prepay-skip at the prior boundary.** An earlier draft skipped the
   upcoming-period in_advance prepay when `cancel_at` fell inside it.
   Rejected — it opens two revenue leaks: (a) `buildCancelLineItems`
   hard-assumes in_advance base was prepaid and skips it entirely, so the
   used stub's base would be billed by nobody (and a scheduled cancel would
   cost less than an immediate cancel at the same instant — arbitrage);
   (b) if the operator unschedules the cancel mid-period, the period runs
   with its base never billed, compounding on re-schedules. Prepaying
   normally and relieving at fire time is uniform with the immediate-cancel
   path and self-consistent under unschedule.

## Crash-safety & idempotency

The flip is the commit point and everything rides its tx: a crash anywhere
before commit leaves the sub `active` with a due `cancel_at`; the scan
re-returns it and the whole composition re-runs. Relief drafts are NOT
independently idempotency-keyed — their safety comes precisely from riding
the CAS-guarded flip tx (a re-entry that loses the CAS never reaches the
draft writes). Any stepwise multi-tx shape was shown unsound in review:
drafts committed before a crash would re-draft (or stall the headroom
allocator loudly, wedging the sub) on re-entry, and a stub invoice
committed before a racing operator immediate-cancel double-bills usage
under two non-colliding period keys.

**The CAS matches `status = 'active' AND cancel_at = <expected>`** — not
status alone. A concurrent `UnscheduleCancel` (clears `cancel_at`) or a
re-schedule to a different instant must defeat the fire: zero rows → the
executor re-reads and returns a clean typed no-op ("unscheduled"), and the
sub simply drops out of the scan's cancel arm. A concurrent operator
immediate-cancel surfaces as `InvalidState` (status no longer active) and
is treated as success — the sub is canceled, and the CAS guarantees exactly
one biller ever ran.

**Scan-arm scoping (livelock guard).** The `OR` arm admits only rows the
executor can make progress on: `s.status = 'active'` in SQL (trialing
cancels belong to the trial scan — ADR-069), parenthesized inside every
existing conjunct (livemode, `test_clock_id IS NULL` on wall queries,
`tc.frozen_time` as the clock query's now). The fetch loops treat a clean
no-op as no-progress only via the failed-set; an admitted row the branch
declines would re-fetch forever — the 2026-05-31 spin bug's shape — so
admission and progress must be the same predicate.

**Zero-stub edge.** `cancel_at == current_billing_period_start` (reachable
via a schedule landing between the loop's refresh and the boundary's cycle
advance) produces no invoice, but relief must still run: the
`prepareCancelCredit` guard widens from strictly-inside to
`>= period_start` so a fully-prepaid, zero-consumed period is fully
relieved rather than silently kept.

## Ties

`cancel_at == next_billing_at` is the boundary path's case
(`shouldFireScheduledCancel` fires at equality after billing the full
period); the mid-period branch requires `cancel_at` strictly before
`next_billing_at`. The two paths cannot both claim an instant.

## Rejected alternatives

- **Reject non-boundary `cancel_at` at schedule time**: honest but BEHIND
  parity (Stripe cancels at arbitrary timestamps) and forces boundary
  arithmetic onto API callers; the firing machinery already existed.
- **Fire at the next boundary after `cancel_at`**: silently bills a period
  the customer canceled — the exact bug, formalized.
- **Bill-then-flip ("fire last")**: both billers hard-require a canceled
  sub (`Status == canceled`, `CanceledAt` set) and no-op silently
  otherwise; synthetic-snapshot workarounds would fork the money path.
