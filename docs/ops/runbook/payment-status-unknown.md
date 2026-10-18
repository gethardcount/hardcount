# Payments stuck as unknown

When Velox charges a card and does not hear the result, it marks the
invoice's payment as `unknown` until Stripe confirms it. This page is for
invoices that stay that way for hours. Back to the
[runbook index](index.md).

## Symptom

Invoices show the `payment_unconfirmed` attention reason for hours. Their
`payment_status` is `unknown`.

## Why

- Stripe's webhook was delayed or lost.
- The reconciler is not running. The reconciler is the sweep that re-checks
  payment status with Stripe on each scheduler tick. See
  [A background job is stuck](background-job-stuck.md).
- The Stripe PaymentIntent (PI) waits at `requires_action`. That is an
  off-session SCA challenge (Strong Customer Authentication, a bank
  verification step) that nobody completes. The reconciler resolves only
  final Stripe outcomes, and skips in-flight PIs on purpose on every sweep.
  So these never resolve on their own; see Fix step 3.

## Check

```sql
SELECT id, payment_status, stripe_payment_intent_id, updated_at
FROM invoices
WHERE payment_status = 'unknown' AND updated_at < now() - interval '1 hour'
LIMIT 20;
```

## Fix

1. Search the application logs for "reconciler" entries. Reconcilers run
   once per scheduler tick (1 hour in production, 5 minutes in local), not
   on a 60-second loop.
2. There is no endpoint to reconcile in bulk. The payment reconciler sweeps
   on its own every tick. For one invoice, use the invoice attention
   actions in the dashboard (charge now, retry). The reconciler's next pass
   also settles any invoice whose PI reached a final state at Stripe.
3. For a PI waiting at `requires_action`: cancel the PI in Stripe (the
   reconciler then settles it as failed), or ask the customer to complete
   authentication.

## Verify

The query above no longer returns the invoice, and the invoice no longer
shows `payment_unconfirmed`.

Related: an attempt that cannot be identified at Stripe at all becomes a
[parked invoice](parked-invoices.md).
