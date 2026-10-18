# Parked invoices

A parked invoice is one whose charge result is unclear, so Velox cannot
tell whether the customer paid. Its charge attempt could not be identified
with the payment provider. This page is for finding out what happened and
closing it. Back to the [runbook index](index.md).

## Symptom

`velox_parked_invoices{mode}` > 0, sustained.

## Why

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
   write-off stops dunning
   ([#976](https://github.com/getvelox/velox/issues/976) removes this case).
3. If nothing was charged, mark the invoice uncollectible. This is the only
   action the invoice page offers. It also releases the invoice's deferred
   clawback draft and closes its dunning run.

## Verify

`velox_parked_invoices{mode}` falls back to zero.

Related: [ADR-107](../../adr/107-unknown-is-terminal-until-a-human.md) and the
[glossary](../../glossary.md).
