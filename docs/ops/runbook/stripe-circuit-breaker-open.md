# Stripe circuit breaker open

A circuit breaker stops calling a service that keeps failing, until it
recovers, like a fuse that trips. Velox has one for Stripe calls. This page
is for when it has tripped, or when payment retries are being skipped for
another reason. Back to the [runbook index](index.md).

## Symptom

- `velox_stripe_breaker_state == 2` (open; 1 = half-open, testing whether
  Stripe has recovered).
- Dunning retries (the automatic retries of failed payments) are skipped.
  A skipped retry does not use up an attempt, adds no row to the invoice
  timeline and sends no email. This is the correct behavior while the cause
  lasts.
- Customers report they expected a retry but no email arrived.

## Why

- The Stripe API has been failing repeatedly (5xx, timeouts, network
  errors), and the breaker tripped to protect the retry budget. The breaker
  is shared by every tenant.
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
   in `Settings → Stripe`, and rotate them if needed.
3. For database errors, restore the database connection first.
4. The breaker resets on its own after a cool-off. Skipped retries run on
   the next dunning pass. Normally no manual action is needed.

## Verify

`velox_stripe_breaker_state` returns to 0 (closed), and new
`skipping this attempt` lines stop.
