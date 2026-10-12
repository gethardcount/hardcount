# ADR-050: Unpaid-source proration — block charges, adjust credits

**Date:** 2026-06-08
**Status:** Accepted

## Context

A mid-period subscription change (plan up/downgrade, quantity change, add/remove
item, cross-interval swap) on an `in_advance` plan prorates against the
current-period prebill invoice. Every Velox proration path resolves that source
invoice and, until now, **silently deferred** when it was finalized but **not
paid** — `handleItemProration` returned `nil` with an info log, and
`BillOnPlanSwapImmediate` skipped its credit. The change still applied; no
invoice, credit, or operator-visible artifact was produced.

A cross-flow audit (2026-06-08) found this is both **inconsistent** and a
**money-correctness bug**:

- **Inconsistent.** Mid-period **cancel** already does the right thing —
  `BillOnCancel` → `relieveUnpaidPrebillOnCancel` *settles* an unpaid prebill by
  voiding it (fully unused) or reducing `amount_due` via an adjustment credit
  note (partially used). But the plan-change / add / remove / quantity / swap
  paths bailed to a silent defer. The same condition produced opposite behavior,
  and the *correct* behavior was the one not reused.
- **Lost credit.** On a **downgrade/removal** with an unpaid source, the deferred
  credit was never re-fired if the invoice was later paid or voided — the
  customer's credit simply vanished. (Violates `feedback_billing_accuracy`.)
- **Invisible.** All defers were info-only — no audit row, no event, no UI.
  (Violates `feedback_no_silent_fallbacks`.)

Industry convergence (verified 2026-06-08 across six platforms; the rule is
unanimous):

- **Stripe** — *"If a customer changes their subscription while having an unpaid
  invoice for the current period, they might receive a credit for unused time on
  the higher-priced plan, even if they haven't paid for that time yet… To avoid
  crediting for unpaid time, you can disable prorations when the subscription's
  latest invoice is unpaid"* (set `proration_behavior=none`). Stripe has **no**
  automatic guard — the integrator must special-case unpaid.
  ([prorations](https://docs.stripe.com/billing/subscriptions/prorations))
- **Chargebee** — *"Adjustment credits reduce the amount a customer owes on an
  invoice that has already been issued but not paid… If the invoice for the
  current term has been paid, then the credits are created as Refundable Credits.
  [Otherwise] we first create Adjustment Credits and reduce the unpaid amount of
  the invoice."* The branch is automatic and accounting-correct.
  ([credit notes](https://www.chargebee.com/docs/billing/2.0/invoices-credit-notes-and-quotes/credit-notes),
  [proration](https://www.chargebee.com/blog/proration/))
- **Recurly** — *"Require that all invoices have been successfully paid in order
  to complete the downgrade. If any invoice is past due, the subscription will be
  kept on the original plan."* Upgrades attempt collection first, else block.
  ([change subscription](https://docs.recurly.com/docs/change-subscription))
- **Lago** — *"prepaid credits and credit notes applied cannot be refunded;
  these can only be credited back to the customer's account balance"*; on
  termination *"any unpaid unused amount is credited back to the customer"*
  (refund only on a succeeded-payment invoice).
  ([credit notes](https://docs.getlago.com/guide/credit-notes))
- **Orb** — an issued-but-unpaid invoice credit note is an **`adjustment`** that
  *"reduce[s] the invoice's amount due directly"*; only a `paid` invoice yields a
  **`refund`** credit note. ([credit notes](https://docs.withorb.com/invoicing/credit-notes))
- **Metronome** — finalized invoices are immutable (void + regenerate); unused
  prepaid value **rolls forward**, never auto-refunds.

The single distilled rule: **never mint a refundable credit, and never stack a
second charge, against money the customer hasn't paid.** Unpaid-source credits
are non-refundable adjustments bound to the open invoice; unpaid-source charges
are gated (block or pay-first), not piled on.

## Decision

One unpaid-source policy, applied at the proration choke point
(`handleItemProration`) and at the engine swap path, branching on the **sign** of
the net proration:

- **Net charge** (upgrade / add-item / quantity-increase) → **block**.
  `settleUnpaidSourceProration` returns `errs.InvalidState` (code
  `unpaid_invoice_blocks_change`, HTTP 409): *"invoice <N> for the current period
  is unpaid (<amount> outstanding); settle or void it before changing this
  subscription."* In production every charge-causing mutation runs the atomic
  proration tx, so the error rolls the item change back — nothing half-applies.
  The atomic wrappers pass `ErrInvalidState` through unwrapped so the operator
  sees the clean message; the legacy non-atomic paths map it to the same 409.

- **Net credit** (downgrade / removal / quantity-decrease) → **adjust in place**.
  Issue a tax-reversing **adjustment credit note** against the open invoice,
  reducing `amount_due`, grossed up by the invoice's `Total/Subtotal` ratio and
  **capped at `amount_due` plus the customer credits applied to the invoice**.
  No cash was funded, so nothing is refunded. *(Amended 2026-10-12, ADR-061.
  The original rule capped at `amount_due` alone and granted no balance credit,
  so customer credits already applied to the invoice were spent on time the
  customer never used. The note now reduces what is due first and returns the
  applied credits the rest of it displaces to the balance.)* This is the exact `CreateAndIssueAdjustment` primitive the unpaid
  **cancel** relief already uses — carried on `ProrationDetail.Clawback*` and
  issued after the tx commits, the same post-commit pattern as the paid clawback
  (ADR-048). `ProrationDetail.Type` gains the value `"adjustment"`.

The engine's `relieveUnpaidPrebillOnCancel` is generalized to
`relieveUnpaidPrebill(reason, desc)` and now also backs
`BillOnPlanSwapImmediate`'s unpaid branch (previously a silent skip), so a
cross-interval swap on an unpaid prebill settles consistently with cancel and the
item-mutation paths.

PAID-source behavior is unchanged: real refundable credit (downgrade) or
proration charge invoice (upgrade).

## Why this design

- **Reuses the correct primitive Velox already had.** `CreateAndIssueAdjustment`
  is payment-status-aware: on a finalized/unpaid invoice it only reduces
  `amount_due` + reverses proportional tax (no refund/credit allocation); on a
  paid invoice it produces a refundable credit. Routing every unpaid-source
  settlement through it makes cancel, swap, and item-mutation byte-consistent by
  construction — the same philosophy as ADR-049's single settlement primitive.
- **Strict superset of the industry rule** (`feedback_stripe_parity_framing`):
  it contains Chargebee's automatic adjust-on-unpaid, Recurly's block-on-past-due
  (for the charge half), Stripe's `none`-on-unpaid recommendation, and Lago/Orb's
  "adjustment, not refund" — as one coherent policy rather than per-path
  special-casing.
- **Fixes the money bug, not the symptom** (`feedback_longterm_fixes`,
  `feedback_billing_accuracy`): the downgrade credit now lands as a real
  `amount_due` reduction immediately, independent of whether the invoice is ever
  paid — it can no longer evaporate.
- **Loud, not silent** (`feedback_no_silent_fallbacks`): the blocked charge is a
  409 the operator sees; the credit is a first-class credit note visible on the
  invoice and credit-note surfaces — the same way the unpaid-cancel relief is
  already surfaced, so no new event type is invented.

## Alternatives considered

- **A. Pay-gated apply (Stripe `pending_update` shape).** Issue the upgrade
  proration invoice and apply the swap only if it's paid. More UX-complete, but a
  whole pending-update state machine — disproportionate for a pre-launch product
  with zero design partners (`feedback_pre_launch_scoping`,
  `feedback_no_overengineering`). Recorded as the post-launch evolution path
  below; block is the simpler correct stance now.
- **B. Visible defer (apply, defer the delta to next cycle, just add audit +
  event).** Lowest friction, but weakest revenue capture and it keeps a
  deferred-credit-that-can-vanish for downgrades. Rejected: it preserves the
  money bug for the credit half.
- **C. Pre-flight guard in the handler before mutating.** Cleaner "no half-apply"
  on the (test-only) non-atomic path, but duplicates the source-lookup + sign
  computation that already live in `handleItemProration`. Rejected as
  belt-and-suspenders (`feedback_no_belt_and_suspenders`): the atomic tx is the
  production mechanism for clean rollback; the choke-point block is the single
  policy location.

## Consequences

### Positive
- Cancel, cross-interval swap, and every item-mutation path settle an unpaid
  source identically — one rule, one primitive.
- Downgrade/removal credit against an unpaid period is captured now (amount_due
  reduced) instead of silently lost.
- Upgrades can't stack a second receivable on an unpaid period; the operator gets
  an actionable 409.

### Risks / open items
- **Operator-UX change:** an upgrade/add/quantity-increase on a sub whose current
  prebill is unpaid now **fails** with a 409 instead of silently applying. This is
  intentional (settle or void the outstanding invoice first) and matches Recurly,
  but it is a behavior change for that edge.
- **Add-item is always a charge**, so adding an item to a sub with an unpaid
  current-period invoice is blocked too — consistent, but worth noting.
- **Cross-cadence swaps** (`in_advance ↔ in_arrears`) remain rejected upstream
  (`service.go`), so they don't reach this policy.
- **Deferred:** pay-gated apply (alternative A) for frictionless upgrades — re-add
  trigger is a design partner who needs to upgrade without first settling an
  outstanding invoice.
- No schema migration.

## References

- ADR-048 (credit-clawback tax reversal — the `CreateAndIssueAdjustment` primitive)
- ADR-049 (single payment-settlement primitive — same discover-then-settle ethos)
- ADR-042 (integer day-ratio proration), ADR-031 (in_advance cancel proration, #22)
- Memory: `feedback_billing_accuracy`, `feedback_no_silent_fallbacks`,
  `feedback_longterm_fixes`, `feedback_stripe_parity_framing`,
  `feedback_audit_overlapping_flows`, `feedback_no_belt_and_suspenders`,
  `feedback_pre_launch_scoping`
- Tests: `TestUpdateItem_Downgrade_UnpaidSource_AdjustsNotRefundableCredit`,
  `TestUpdateItem_Downgrade_UnpaidSource_CapsAtAmountDue`,
  `TestUpdateItem_Upgrade_UnpaidSource_Blocked`,
  `TestBillOnPlanSwapImmediate` (unpaid-adjustment subtest),
  `TestBillOnCancel_UnpaidPrebillRelief`
