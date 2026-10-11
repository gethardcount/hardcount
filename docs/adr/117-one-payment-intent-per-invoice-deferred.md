# ADR-117: One PaymentIntent per invoice — considered, deferred

**Date:** 2026-10-11
**Status:** Proposed (deferred with triggers)

## Summary

Stripe Billing gives each invoice one PaymentIntent, created at finalize,
and every way of paying confirms that same one. Velox creates a new
PaymentIntent for every charge attempt and another for each hosted Checkout
session. One PaymentIntent per invoice would make "at most one payment
succeeds" a property of Stripe rather than of Velox's code. We are not
adopting it now: the problems it would remove are already closed by narrower
fixes, and the move would rebuild the hosted page, the idempotency model and
the reconcilers. This ADR records the option, its cost, and the signals that
should reopen it.

## Context

**What Velox does.** Each off-session charge (auto-charge, dunning retry,
operator Collect) creates a PaymentIntent whose idempotency key is seeded by
`invoices.charge_attempt_seq` (ADR-105), so each genuine retry gets a new
PaymentIntent. The hosted Pay page opens a Stripe Checkout Session in
`mode=payment`, and Checkout creates its own PaymentIntent (ADR-021,
ADR-068). One invoice can therefore have many PaymentIntents, and more than
one could succeed.

**What that has cost.** Several decisions exist only because several
PaymentIntents can report on one invoice:

- A hosted payment and a Velox charge could both take money. This is closed
  by the "one payer at a time" claim exclusion (ADR-068 amendment, #929).
- A late decline from an older attempt could overwrite a newer one. This is
  closed by the failure-ordering rule `PaymentFailureReport.MovesInvoice`
  (ADR-049 amendment, #928).
- An ambiguous attempt with no PaymentIntent id has to be parked and later
  searched for and adopted (ADR-107, ADR-108).
- A second success is detected after the fact and raised as
  `payment.duplicate_charge` for the operator to refund (ADR-068).

**What peers do.** They split.

- **Stripe Billing: one PaymentIntent per invoice.** Finalizing an invoice
  "creates an incomplete payment intent for the invoice" ([Status transitions
  and finalization](https://docs.stripe.com/invoicing/integration/workflow-transitions)).
  Automatic collection and the hosted invoice page both complete that
  PaymentIntent.
- **Lago: one PaymentIntent per payment record**, the same model as Velox:
  `::Stripe::PaymentIntent.create(payload, { …, idempotency_key: "payment-#{payment.id}" })`
  ([lago-api create_service.rb](https://github.com/getlago/lago-api/blob/main/app/services/payment_providers/stripe/payments/create_service.rb)).
- **Kill Bill: per payment transaction.** The Stripe plugin keeps each
  transaction's outcome separately, with the Kill Bill transaction id in the
  PaymentIntent's metadata. Verified in the 2026-10-11 peer check.

## Decision

Keep one PaymentIntent per charge attempt and per hosted session. Do not
migrate now. Reopen this ADR when a trigger below fires; until then, the
narrower mechanisms listed in Context are the answer.

## Why this design

Velox already enforces the invariant that matters: two payment channels
never take money for one invoice at the same time. Each known failure has a
targeted fix, a real-Postgres test, and a walk against real Stripe.

One PaymentIntent per invoice would move that invariant into Stripe, which
is cleaner. But it needs the changes below, each one a project of its own,
and none of them is needed today:

- **The hosted page.** A Checkout Session cannot attach to an existing
  PaymentIntent; it always creates its own. The page would move to Stripe's
  Payment Element confirming the invoice's PaymentIntent with its client
  secret. That means Velox owns the payment UI, 3D Secure handling and
  wallet display that Checkout gives for free.
- **Amount changes.** A credit note or credit application that changes the
  amount due must update the PaymentIntent's amount before any
  confirmation, under the same lock the charge claims take.
- **Idempotency.** Retries would re-confirm one PaymentIntent with a payment
  method, not create a new one. ADR-105's per-attempt key, the attempt
  history (ADR-102) and the failure-ordering rule (#928) would all be
  rebuilt around one PaymentIntent's sequence of attempts.
- **Reconcilers.** Park, search and adopt (ADR-107/108) assume many
  PaymentIntents per invoice and would be rewritten.

Peers do not settle the question either. Lago and Kill Bill run the
per-attempt model in production, as Velox does.

## Alternatives considered

- **A. Adopt now.** This rebuilds the hosted page and every payment
  reconciler to remove failure classes that are already closed and walked.
  Rejected: the cost is large, and the benefit today is structural tidiness,
  not a missing guarantee.
- **B. One PaymentIntent for off-session attempts only, Checkout
  unchanged.** This would simplify retry ordering but leave the hosted page
  as a second payer, so ADR-068's exclusion stays. Rejected: most of the
  cost of A for a small part of the benefit.

## Consequences

### Positive
- No migration risk. The current invariants, tests and walks stay valid.
- The option, its cost and its triggers are written down, so the next
  person who sees "Stripe uses one PaymentIntent per invoice" can start here.

### Risks / open items
- The per-attempt model keeps its accepted residuals. The ADR-068
  amendment's offline-payment residual and ADR-107's parked invoices remain,
  each registered with its own trigger.
- **Triggers to reopen:**
  - A double payment, or a wrong attempt's outcome recorded, in production
    that the current mechanisms did not catch.
  - The hosted page moving to the Payment Element for another reason (for
    example wallets or bank debits that Checkout cannot offer as configured).
    The largest cost above is then already paid.
  - A second payment provider. One payment object per invoice would then be
    a modelling question for every provider, not a Stripe detail.

## References

- ADR-021: the hosted Pay page saves the card.
- ADR-049 and its 2026-10-11 amendment: failure ordering (#928).
- ADR-068 and its 2026-10-11 amendment: one payer at a time (#929).
- ADR-102: charge attempts. ADR-105: the attempt idempotency key.
- ADR-106: the charge-intent ledger (parked). ADR-107: park the ambiguous
  charge. ADR-108: search and adopt.
- [Stripe: Status transitions and finalization](https://docs.stripe.com/invoicing/integration/workflow-transitions)
- [Lago: Stripe payments create service](https://github.com/getlago/lago-api/blob/main/app/services/payment_providers/stripe/payments/create_service.rb)
