# ADR-088: Customer credit balance applies to every invoice when it is collected, card charged the remainder

**Status:** Accepted (2026-07-11; amended 2026-10-08, 2026-10-19)
**Context:** ADR-087 recorded credit-apply presence as a per-site product
decision (census D2, trap R1) rather than a refactor side-effect. This ADR is
that decision, grounded in a 103-agent adversarially-verified survey of
primary vendor docs (Stripe, Chargebee, Lago, Orb, Metronome — all claims
3-0 verified, 2026-07-11).

## Summary

A customer's credit balance applies automatically to every invoice: cycle close, threshold fires, a subscription's first invoice, the final invoice on cancel, and one-off manual invoices. The card is charged only what is left, and an invoice the balance fully covers is marked paid with no charge. There is no per-invoice or per-type opt-out, which matches Stripe, and no vendor surveyed skips a subscription's first or final invoice.

The credit is applied when the invoice is collected, not when it is finalized. The invoice is finalized and `invoice.finalized` is sent first; the one collector (`processAutoCharge`, [ADR-116](116-collection-intent-at-finalize-one-collector.md)) then applies credit right before any charge, usually within seconds of finalize. Every later attempt to collect an unpaid invoice applies credit again, so credit granted later can pay an older open invoice. So `amount_due_cents` in `invoice.finalized` is the amount before credit, and the payload carries `credits_applied_cents`. Amended 2026-10-19, see [the amendment](#amendment-2026-10-19-credit-is-applied-when-the-invoice-is-collected). Status: active, amended; prepaid commits ([ADR-078](078-prepaid-commits-phase-1.md)) keep their own rules.

## Decision

*Amended 2026-10-19: credit is applied when the invoice is collected, not at
finalize. See [the amendment](#amendment-2026-10-19-credit-is-applied-when-the-invoice-is-collected).
The text below is the 2026-07-11 decision.*

The customer credit balance auto-applies to **all** invoice types at
finalize-time amount computation — day-1 subscription-create, cancellation
final, and operator-composed manual invoices join the existing cycle-close and
threshold sites. The card is charged only the post-credit remainder; a fully
covered invoice settles paid with no payment attempt. No per-invoice or
per-type opt-out.

## Industry evidence (verified quotes, primary docs)

- **Stripe** applies the balance "to the next invoice finalised for the
  customer" — and its own definition of that population includes "manually
  create a one-off invoice." No per-invoice opt-out exists in GA ("You can't
  choose a specific invoice to apply the credit balance to"); partial coverage
  charges exactly the remainder ("The charge that gets generated for the
  invoice will be for the amount specified in amount_due"); full coverage
  auto-pays with no charge. (docs.stripe.com/billing/customer/balance,
  /invoicing/customer/balance, /api/invoices/object)
- **Chargebee** auto-applies at invoice generation to "all recurring and
  one-time invoices"; it is the only surveyed platform with per-invoice-type
  toggles (Credits Flexibility, Beta).
- **Lago** applies wallet credits to all subscription invoices at generation
  but **categorically excludes one-off invoices** with no override.
- **Orb** applies prepaid credits inside invoice calculation (pre-tax) but
  only to in-arrears charges — its credits are usage-commitment-flavored, a
  different primitive from a general balance.
- **Metronome** applies credits continuously against the draft.

Parity synthesis: **no platform excludes a subscription's first or final
invoice** from automatic application — Velox charging full price there was out
of parity with every surveyed vendor. One-off invoices split the industry
(Stripe+Chargebee apply, Lago excludes); Velox anchors on Stripe.

## Why apply at manual invoices too (the split call)

Stripe parity is Velox's stated anchor, and the two largest platforms agree.
The customer-visible logic is also simpler to explain: "your balance pays your
invoices" with no invoice-type asterisk. Rejected alternative: Lago-style
exclusion (shippable, but creates the support question "why didn't my credit
apply?"); deferred alternative: a Chargebee-style per-type toggle —
over-engineering pre-launch, trigger = the first design partner asking to
reserve balance for subscription charges only.

## Velox-specific boundaries

- The **credit ledger** is a general balance (refunds, goodwill,
  cancel-proration credits) — the Stripe model. **Prepaid commits (ADR-078)**
  are a separate line-item primitive and keep their own semantics; Orb's
  in-arrears-only rule applies to *its* commitment credits and is not imported
  here.
- Application happens at finalize-time amount computation (Stripe-exact:
  Velox's finalize step is the analogue of Stripe finalization), never at
  payment time, and never retroactively to already-finalized invoices.
  *Superseded 2026-10-19: credit is applied at collection, and again on each
  later collection attempt. See [the amendment](#amendment-2026-10-19-credit-is-applied-when-the-invoice-is-collected).*

## Implementation shape (trap R1 honored)

The new sites reuse the retry sweep's proven sequence — apply → reload →
settle-if-zero → collect — not billOnePeriod's block (whose $0 arm is
entangled with cycle-advance, trap R2):

*Amended 2026-10-08 ([ADR-116](116-collection-intent-at-finalize-one-collector.md)):
the per-site blocks below are gone. Every finalize site collects through one
collector (`processAutoCharge`), which applies credits before any charge on
every invoice. The policy of this ADR is unchanged; the mechanism below is
history.* *Corrected 2026-10-19: the timing changed too. Credit moved from
finalize to collection, see [the amendment](#amendment-2026-10-19-credit-is-applied-when-the-invoice-is-collected).*

- Engine: `applyCreditsAndCollect` wraps `collectAfterFinalize` for the day-1
  (`FinalizeOnCreateInvoice`) and final-on-cancel sites. An apply failure
  **flags for the sweep and returns without charging** — never a pre-credit
  card charge (the 2026-05-30 overcharge class); the sweep already re-applies
  credits atomically before charging, so recovery pre-exists.
- Manual finalize: the handler applies via a consumer-defined `CreditApplier`
  (wired to credit.Service in the composition root), then its existing
  zero-due settle arm handles full coverage; apply failure flags for the
  sweep, same as the engine.
- Cycle-close and threshold keep their existing in-place blocks (deliberate:
  behavior-preserving; billOnePeriod's is cycle-advance-entangled). Collapsing
  them onto `applyCreditsAndCollect` is a later, separate simplification.

## Amendment 2026-10-19: credit is applied when the invoice is collected

**What was wrong.** This ADR said credit applies at finalize, "never at
payment time, and never retroactively to already-finalized invoices". The
2026-10-08 amendment called the policy unchanged. But ADR-116 moved the
credit step into the collector, and the code has worked like this since:

- The invoice is finalized and `invoice.finalized` is sent. Then the
  collector applies credit, right before it settles or charges. The nudge
  after finalize runs it within seconds; the hourly sweep runs it for an
  invoice that is still queued.
- Credit is applied again on every later attempt to collect an unpaid
  invoice: each sweep visit to a queued invoice, each dunning retry, and an
  operator's **Collect payment** (`POST /v1/invoices/{id}/collect`). So
  credit granted later can pay an older open invoice on its next attempt.
  ADR-116 relies on this: the sweep's visit "settles it when credits arrive".

**Decision.** Keep the code's behaviour and fix the record (decided
2026-10-18 on [#1281](https://github.com/getvelox/velox/issues/1281)).
Applying credit at collection means new credit pays what the customer
already owes at the next attempt, before the card is charged again. Stripe
applies its balance "to the next invoice finalised for the customer" (quoted
above); Velox differs from Stripe on purpose here.

**What integrators see.** `invoice.finalized` is sent before the credit
step, so its `amount_due_cents` is the amount before credit. Its payload now
carries `credits_applied_cents`, read from the invoice, so the two add up as
they do on the invoice. For an invoice made by the engine it is normally 0.
No event is sent when credit lowers `amount_due_cents` without paying the
invoice in full. `invoice.paid` is sent when credit covers all of it. To
know what is still owed, read the invoice (`GET /v1/invoices/{id}`).

**Considered and rejected: send `invoice.finalized` after the credit step.**
The event would then wait for the collector. The collector may charge the
card in the same run, so `invoice.paid` could arrive before
`invoice.finalized`. Some invoices are not collected right away (a proration
invoice waits for the sweep; another collector may hold the invoice's
lease), so the event would be late or need a second sender. And it would
still miss credit applied on a later attempt. The payload field has none of
these costs. With it, and with the docs saying what `amount_due_cents` means
there, a receiver knows the figure is before credit and reads the invoice for
what is still owed.
