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

- Stripe calls got no clear answer 5 times in a row: a 5xx, a timeout or a
  dropped connection. Declines and other definite errors do not count, and
  neither does a rejected API key.
- The breaker is one per Velox process, shared by every tenant. One
  tenant's calls can open it for all of them on that replica.

## Check

A skipped retry writes no row: the attempt is not counted, so it is not
recorded. Find the skips in the logs:

```bash
grep "dunning retry skipped — upstream transient" log | jq '{run_id, invoice_id}'
```

```sql
-- Retries that reached Stripe in the last hour
SELECT count(*) FROM invoice_dunning_events
WHERE event_type = 'retry_attempted' AND created_at > now() - interval '1 hour';
```

## Fix

1. Check Stripe's status page (`status.stripe.com`).
2. The breaker closes on its own: after a 30-second cool-off it lets one
   call through, and a success closes it. Normally no manual action is
   needed.

## Verify

`velox_stripe_breaker_state` returns to 0 (closed), the skip lines stop,
and due retries write `retry_attempted` rows again.
