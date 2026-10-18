# Stripe circuit breaker open

A circuit breaker stops calling a service that keeps failing, until it
recovers, like a fuse that trips. Velox has one for Stripe calls. This page
is for when it has tripped and payment retries are being skipped. Back to
the [runbook index](index.md).

## Symptom

- `velox_stripe_breaker_state == 2` (open; 1 = half-open, testing whether
  Stripe has recovered).
- Dunning retries (the automatic retries of failed payments) are skipped
  without a message. This is the correct behavior while the breaker is
  open.
- Customers report they expected a retry but no email arrived.

## Why

- The Stripe API has been failing repeatedly, and the breaker tripped to
  protect the retry budget.
- The tenant's Stripe credentials are invalid. The breaker is per tenant.

## Check

```sql
-- Recent payment retry outcomes
SELECT outcome, count(*) FROM (
  SELECT
    CASE WHEN reason LIKE '%breaker%' OR reason LIKE '%transient%'
         THEN 'transient_skip'
         ELSE 'real_failure' END AS outcome
  FROM invoice_dunning_events
  WHERE event_type = 'retry_attempted' AND created_at > now() - interval '1 hour'
) t GROUP BY outcome;
```

## Fix

1. Check Stripe's status page (`status.stripe.com`).
2. If only one tenant is affected, check its Stripe credentials in
   `Settings → Stripe`, and rotate them if needed.
3. The breaker resets on its own after a cool-off. Normally no manual
   action is needed.

## Verify

`velox_stripe_breaker_state` returns to 0 (closed), and the query above
shows `real_failure` or successful retries instead of `transient_skip`.
