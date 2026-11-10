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
  lasts. The one exception: if Hardcount could not take the attempt back
  (the database was still failing), the retry is counted and is not run
  again. See Check.
- Customers report they expected a retry but no email arrived.

## Why

- Stripe calls got no clear answer 5 times in a row (a 5xx, a timeout or a
  dropped connection), and the breaker tripped, so payment retries are not
  spent while Stripe is failing.
  Declines and other definite errors do not count, and neither does a
  rejected API key.
- The breaker is one per Hardcount process, shared by every account. One
  account's calls can open it for all of them on that replica.
- Stripe is rate limiting, or an account's Stripe credentials could not be
  read. These skip retries without tripping the breaker.
- A database error during the retry, such as a dropped connection. The
  readiness probe shows those: `/health/ready` answers 503 while the
  database cannot be reached ([monitoring](monitoring.md#page-critical)).

## Check

Search the logs for `skipping this attempt`. Each `WARN` line names the
invoice and the error. A Stripe error or `payment attempt skipped` points to
Stripe; a connection or `SQLSTATE` error points to the database.

Search also for `failed to rewind the attempt recorded before the charge`.
Each `ERROR` line names a retry that was counted although it was skipped.
That run does not retry on the next pass: it waits for its next scheduled
retry, or, when the line says `final_attempt=true`, goes to the policy's
final action without that charge.

## Fix

1. Check Stripe's status page (`status.stripe.com`).
2. If only one account's retries are skipped, check its Stripe credentials
   in `Settings → Payments`, and rotate them if needed.
3. For database errors, restore the database connection first.
4. The breaker closes on its own: after a 30-second cool-off it lets one
   call through, and a success closes it. Skipped retries run on the next
   dunning pass, except one the logs name as counted (see Check). Normally
   no manual action is needed. For a counted last retry, use **Collect
   payment** on the invoice before the final action runs, about 15 minutes
   after the log line, if you want that charge made.

## Verify

`hardcount_stripe_breaker_state` returns to 0 (closed), and new
`skipping this attempt` lines stop.
