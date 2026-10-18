# Parked invoices

A parked invoice is one whose charge result is unclear, so Velox cannot
tell whether the customer paid. Its charge attempt could not be identified
with the payment provider. This page is for finding out what happened and
closing it. Back to the [runbook index](index.md).

## Symptom

`velox_parked_invoices{mode}` > 0, sustained.

## Why

- An invoice parks in two ways
  ([ADR-127](../../adr/127-record-each-charge-before-calling-stripe.md)):
  - Stripe answered a charge with 409 "same key, different parameters". Only
    a charge in flight during an upgrade gets this answer.
  - The reconciler could not read Stripe about an interrupted charge for 24
    hours. The CRITICAL's `error` says "Stripe could not be read about this
    charge".

  Other unclear answers do not park: see
  [Interrupted charges](interrupted-charges.md).
- A parked invoice cannot be charged, on purpose. No sweep, no dunning
  retry and no operator "Collect payment" will touch it. That is what makes
  a double charge impossible.
- **It resolves on its own only when Stripe's answer reaches Velox.** That
  is the charge's webhook, paid or declined, or Velox's hourly search in
  Stripe, which adopts a payment that succeeded or is still running. The
  search never records a decline. Otherwise it needs a human.
- Alert on presence, not growth. With no customers affected the value is
  zero, and each one is a real invoice a real customer cannot pay.

## Check

Find the attempt in Stripe by customer, amount and approximate time.

## Fix

1. If the charge succeeded, its webhook or the hourly search settles the
   invoice. No action is needed.
2. If the charge was declined, its `payment_intent.payment_failed` webhook
   marks the invoice failed and dunning starts. If the invoice is still
   parked, Velox did not apply that webhook. Fix the webhook endpoint if it
   is failing, then send the event again from the Stripe dashboard. If the
   invoice stays parked, Velox had already received that event. Mark the
   invoice uncollectible and ask the customer to pay another way: the
   write-off stops dunning. This case came from an unclear charge answer
   that parked; since ADR-127 such an answer keeps a charge record instead,
   and the reconciler records the decline.
3. If nothing was charged, mark the invoice uncollectible. This is the only
   action the invoice page offers. It also releases the invoice's deferred
   clawback draft and closes its dunning run.
4. A write-off in step 2 or 3 closes the invoice's dunning run without the
   policy's final action (cancel or pause the subscription). Check whether
   that action was due:
   - Find the run with `GET /v1/dunning/runs?invoice_id=<invoice id>`, then
     the policy it runs on with `GET /v1/dunning/policies/<policy_id>`. A run
     keeps the policy it started on, even if the customer moved to another.
   - If the run's `attempt_count` has reached the policy's
     `max_retry_attempts` and its `final_subscription_action` is `cancel` or
     `pause`, apply that action by hand on the subscription page. Dunning
     held it because a payment was in flight.
   - With no run, or with retries left, the final action was never due. The
     invoice was parked before dunning started or before its last retry.

## Verify

`velox_parked_invoices{mode}` falls back to zero.

Related: [ADR-107](../../adr/107-unknown-is-terminal-until-a-human.md) and the
[glossary](../../glossary.md).
