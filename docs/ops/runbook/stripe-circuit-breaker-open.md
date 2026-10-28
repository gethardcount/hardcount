# Stripe circuit breaker open

A circuit breaker stops calling a service that keeps failing, until it
recovers. Hardcount has one for Stripe calls. This page also covers payment
retries skipped for another reason. Back to the [runbook index](index.md).

## Symptom

- `hardcount_stripe_breaker_state == 2` (open; 1 = half-open, testing whether
  Stripe has recovered).
- Dunning retries (the automatic retries of failed payments) are skipped.
  A skipped retry does not use up an attempt, adds no row to the invoice
  timeline and sends no email. This is the correct behavior while the cause
  lasts.
- Customers report they expected a retry but no email arrived.

## Why

- Stripe calls got no clear answer 5 times in a row (a 5xx, a timeout or a
  dropped connection), and the breaker tripped to protect the retry budget.
  Declines and other definite errors do not count, and neither does a
  rejected API key.
- The breaker is one per Hardcount process, shared by every tenant. One
  tenant's calls can open it for all of them on that replica.
- Stripe is rate limiting, or a tenant's Stripe credentials could not be
  read. These skip retries without tripping the breaker.
- A database error during the retry, such as a dropped connection. Postgres
  connection errors have their own alert ([monitoring](monitoring.md)).

## Check

Search the logs for `skipping this attempt`. Each `WARN` line names the
invoice and the error. A Stripe error or `payment attempt skipped` points to
Stripe; a connection or `SQLSTATE` error points to the database.

## Fix

1. Check Stripe's status page (`status.stripe.com`).
2. If only one tenant's retries are skipped, check its Stripe credentials
   in `Settings → Payments`, and rotate them if needed.
3. For database errors, restore the database connection first.
4. The breaker closes on its own: after a 30-second cool-off it lets one
   call through, and a success closes it. Skipped retries run on the next
   dunning pass. Normally no manual action is needed.

## Verify

`hardcount_stripe_breaker_state` returns to 0 (closed), and new
`skipping this attempt` lines stop.
