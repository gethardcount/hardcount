# Parked invoices

A parked invoice is one whose last charge got an unclear answer from Stripe
(a 5xx, a 409, a network error, or a request past the caller's time limit)
that named no payment. So Velox cannot yet tell whether the customer paid.
Velox finds out on its own within a day; this page is for the parks it
cannot finish. Back to the [runbook index](index.md).

## Symptom

`velox_parked_invoices{disposition="open"}` above 0. The gauge counts only
parks Velox has not ended on schedule: still parked 26 hours after the
charge's hold, or with no hold. Parks inside their 24 hours are not counted.

## Why

- A parked invoice is not charged while the answer is unclear, on purpose:
  no sweep, no dunning retry, no **Collect payment** and no hosted page.
  That is what keeps the customer from paying twice.
- Every billing tick (hourly), Velox lists the customer's payments in Stripe.
  A payment for the invoice that succeeded marks it paid; one still
  processing is followed until it ends. The charge's webhook can settle it
  sooner.
- If no payment has appeared 24 hours after the attempt, the invoice goes
  back to `pending` (`failed` after a dunning retry) and is charged again as
  usual. Stripe can still create a payment that long after a 5xx, while it
  recovers from an incident, so Velox waits that long before charging again.
- So a counted park is one the check could not finish. Either Velox cannot
  ask Stripe about it: the Stripe connection is broken (key revoked or
  rejected), or the customer is gone from the Stripe account. The log has an
  ERROR on every check: `stale charge check: can't confirm an earlier charge
  with Stripe`. Or Stripe could not be read on any check for a day. Or the
  park has no hold, so nothing checks it: a replica of an older release
  cleared it during a rolling upgrade.

## Check

1. Fix the Stripe connection if it is broken. The next tick finishes the
   park on its own.
2. Find the attempt in the Stripe dashboard by customer, amount and
   approximate time.
3. Check that the invoice has a hold:
   `SELECT auto_charge_claimed_until FROM invoices WHERE id = '<invoice id>';`
   With none, give it one dated a day back, and the next tick finishes the
   park:

   ```sql
   BEGIN;
   SET LOCAL app.bypass_rls = 'on';
   UPDATE invoices SET auto_charge_claimed_until = now() - interval '25 hours'
   WHERE id = '<invoice id>' AND payment_status = 'unknown'
     AND auto_charge_claimed_until IS NULL;
   COMMIT;
   ```

## Fix

1. If the charge succeeded and the park does not end (the customer is
   deleted in Stripe, so Velox cannot ask), send its
   `payment_intent.succeeded` event again from the Stripe dashboard to the
   Velox webhook endpoint. The webhook marks the invoice paid, a written-off
   one too.
2. If nothing was charged and the park does not end, or the customer paid
   another way, mark the invoice uncollectible. It is the one action the
   invoice page allows on a parked invoice. It also releases the invoice's
   deferred clawback draft and closes its dunning run.
3. A write-off closes the invoice's dunning run without the policy's final
   action (cancel or pause the subscription). Check whether that action was
   due:
   - Find the run with `GET /v1/dunning/runs?invoice_id=<invoice id>`, then
     the policy it runs on with `GET /v1/dunning/policies/<policy_id>`. A run
     keeps the policy it started on, even if the customer moved to another.
   - If the run's `attempt_count` has reached the policy's
     `max_retry_attempts` and its `final_subscription_action` is `cancel` or
     `pause`, apply that action by hand on the subscription page. Dunning
     held it because a payment was in flight.
   - With no run, or with retries left, the final action was never due. The
     invoice was parked before dunning started or before its last retry.

A customer who paid another way inside the 24 hours: mark the invoice
uncollectible before the 24 hours end, or Velox charges the card again.

## Verify

`velox_parked_invoices{disposition="open"}` falls back to zero.

Related: [card payment failure handling](../../payments.md),
[ADR-127](../../adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md),
[ADR-107](../../adr/107-unknown-is-terminal-until-a-human.md) and the
[glossary](../../glossary.md).
