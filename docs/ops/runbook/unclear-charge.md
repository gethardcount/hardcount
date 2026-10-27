# A charge's result is unclear

Velox sent a charge to Stripe and does not know yet whether the customer
paid. Until it knows, it does not charge that invoice again, so the customer
is never charged twice. Most of these end on their own. Find your case, then
follow its Check, Fix and Verify. Back to the [runbook index](index.md).

| What you see | Case |
|---|---|
| Invoices show `payment_unconfirmed` for hours, and the invoice has a Stripe PaymentIntent | [Unconfirmed, with a PaymentIntent](#unconfirmed-with-a-paymentintent) |
| `velox_parked_invoices{disposition="open"}` above 0 | [Parked: no PaymentIntent](#parked-no-paymentintent) |
| `stale charge check: can't confirm an earlier charge with Stripe` in the logs, or `velox_stale_charge_hold_resolutions_total{result="cannot_ask"}` rising | [Interrupted: Velox cannot ask Stripe](#interrupted-velox-cannot-ask-stripe) |
| `velox_creditnote_pending_issue_drafts` growing over days | [Clawback drafts waiting](#clawback-drafts-waiting) |

Velox normally ends a park within a day: with no payment at Stripe 24 hours
after the attempt, the invoice is charged again as usual.

## Unconfirmed, with a PaymentIntent

**Symptom:** invoices show the `payment_unconfirmed` attention reason for
hours. Their `payment_status` is `unknown`, and they have a
`stripe_payment_intent_id`.

**Why:**

- Stripe's webhook was delayed or lost.
- The reconciler is not running. The reconciler re-checks payment status
  with Stripe on each scheduler tick. See
  [A background job is stuck](background-job-stuck.md).
- The Stripe PaymentIntent (PI) is still not final at Stripe (for example
  `requires_action`). The reconciler settles only final outcomes, so it
  records what it saw and skips it every pass.

**Check:**

```sql
SELECT id, payment_status, stripe_payment_intent_id, updated_at
FROM invoices
WHERE payment_status = 'unknown' AND stripe_payment_intent_id <> ''
  AND updated_at < now() - interval '1 hour'
LIMIT 20;
```

Search the application logs for `reconciler` lines. It runs once per
billing tick.

**Fix:**

There is no bulk or per-invoice re-check, and no dashboard action to charge
it again. Velox refuses to charge an invoice whose payment is `unknown`
(**Collect payment** answers 409), so it is never charged twice. The
reconciler re-checks it every tick and settles any PI that has reached a
final state at Stripe.

To end it sooner, cancel the PI in Stripe. The reconciler then records it as
failed.

**Verify:** the query above no longer returns the invoice, and the invoice
no longer shows `payment_unconfirmed`.

## Parked: no PaymentIntent

A parked invoice is one whose last charge got an unclear answer from Stripe
(a 5xx, a 409, a network error, or a request past the caller's time limit)
that named no payment. So Velox cannot yet tell whether the customer paid.

**Symptom:** `velox_parked_invoices{disposition="open"}` above 0. The gauge
counts only parks Velox has not ended on schedule. Parks less than 26 hours
past their hold are not counted: Velox normally ends them by about 25 hours.
A park with no hold is always counted.

**Why:**

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
  ask Stripe about it (see [Interrupted](#interrupted-velox-cannot-ask-stripe)),
  or Stripe could not be read on any check for a day. Or the park has no
  hold, so nothing checks it; Check step 3 gives it one.

**Check:**

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

**Fix:**

1. If the charge succeeded and the park does not end (the customer is
   deleted in Stripe, so Velox cannot ask), send its
   `payment_intent.succeeded` event again from the Stripe dashboard to the
   Velox webhook endpoint. The webhook marks the invoice paid, a written-off
   one too.
2. If nothing was charged and the park does not end, or the customer paid
   another way, mark the invoice uncollectible. It is the one action the
   invoice page allows on a parked invoice. It closes its dunning run. A
   credit note draft held on the invoice still waits until the park ends.
3. A write-off closes the invoice's dunning run without the policy's final
   action (cancel or pause the subscription). Check whether that action was
   due:
   - Find the run with `GET /v1/dunning/runs?invoice_id=<invoice id>`, then
     the policy it runs on with `GET /v1/dunning/policies/<policy_id>`. A run
     keeps the policy it started on, even if the customer moved to another.
   - If the run's `attempt_count` has reached its `max_retry_attempts` (the
     number of entries in the policy's `retry_days`) and the policy's
     `final_subscription_action` is `cancel` or
     `pause`, apply that action by hand on the subscription page. Dunning
     held it because a payment was in flight.
   - With no run, or with retries left, the final action was never due. The
     invoice was parked before dunning started or before its last retry.

A customer who paid another way inside the 24 hours: mark the invoice
uncollectible before the 24 hours end, or Velox charges the card again.

**Verify:** `velox_parked_invoices{disposition="open"}` falls back to zero.

## Interrupted: Velox cannot ask Stripe

A charge is interrupted when Velox sent it to Stripe but did not save the
answer (a crash, a shutdown, a closed **Collect payment**, or a 429 on a
resend). Velox holds the invoice and asks Stripe before charging again
([ADR-127](../../adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)).
This section is for when it cannot ask.

**Symptom:**

- This ERROR in the logs, every hour for the same invoice:
  `stale charge check: can't confirm an earlier charge with Stripe; write the invoice off or fix the Stripe connection`.
- `velox_stale_charge_hold_resolutions_total{result="cannot_ask"}` rising.
- An invoice stays Pending or Failed. **Collect payment**, void and
  offline payment answer: "A charge on this invoice is being confirmed with
  Stripe." Dunning does not retry it, and its final action waits.

A steady `velox_stale_charge_hold_resolutions_total{result="failed"}` is
Stripe being unreachable; the check retries and needs nothing from you
unless it lasts. When three checks in a row get no answer from Stripe, the
check stops until the next hour and logs `stale charge check: Stripe could
not be reached several times in a row`.

**Why:** the reconciler lists the customer's payments in Stripe to learn
what the interrupted charge did. It cannot when:

- the tenant disconnected Stripe for this mode (live or test);
- Stripe rejects the tenant's key (revoked or rotated), or the key is a
  restricted key without permission to read PaymentIntents;
- the Stripe customer is not in this Stripe account (deleted there, or
  created under other credentials).

Until it can ask, Velox will not charge the invoice again: the interrupted
charge may have taken the money.

**Check:**

```sql
-- Invoices whose charge hold ended more than 10 minutes ago
SELECT i.id, i.tenant_id, i.invoice_number, i.status, i.payment_status,
       i.auto_charge_claimed_until, i.provider_synced_at, c.stripe_customer_id
FROM invoices i
LEFT JOIN customers c ON c.id = i.customer_id
WHERE i.auto_charge_claimed_until < now() - interval '10 minutes'
ORDER BY i.auto_charge_claimed_until;
```

In the Stripe dashboard, open the customer (`stripe_customer_id` above) and
look for a payment whose metadata `velox_invoice_id` is the invoice's id.

**Fix:**

1. **Stripe disconnected or key rejected:** reconnect Stripe or replace the
   key under Settings → Payments. The next hourly check then finishes on
   its own.
2. **The customer is gone from Stripe, or Stripe will not be reconnected:**
   on an open invoice the check never clears, and offline payment is refused
   while it waits. In this order:
   1. **Mark uncollectible** (write-off). It moves no money and is allowed
      while the check waits.
   2. Wait for the next check. It clears the claim on the written-off
      invoice, with one more ERROR. A parked invoice keeps its claim, with
      an ERROR every check, until Stripe can be asked: see
      [Parked](#parked-no-paymentintent).
   3. If Stripe shows a payment for this invoice, record it with **Record
      payment**. That moves the invoice from uncollectible to paid.

Do not clear `auto_charge_claimed_until` by hand: that is what lets the next
charge go out, and the interrupted one may have succeeded.

**Verify:**

- The query above no longer lists the invoice.
- `velox_stale_charge_hold_resolutions_total` shows `settled`, `released` or
  `cleared` for the run that resolved it.

## Clawback drafts waiting

A clawback is a credit note Velox issues when a paid, in-advance period is
cut short, for example by a downgrade. Velox writes it as a draft first, and
a draft whose source payment is still unclear waits for it.

**Symptom:** `velox_creditnote_pending_issue_drafts` grows over days.

**Check and fix:** is `velox_parked_invoices{disposition="open"}` above
zero? If so, follow [Parked](#parked-no-paymentintent); each draft is
released when its park ends. Otherwise the drafts are waiting on source
payments still in flight, including parks inside their 24 hours, and need
no action.

**Verify:** the drafts drain on the next reconciler pass.

Related: [card payment failure handling](../../payments.md),
[ADR-127](../../adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md),
[ADR-107](../../adr/107-unknown-is-terminal-until-a-human.md),
[ADR-059](../../adr/059-guard-invoice-mutations-while-payment-in-flight.md)
and the [glossary](../../glossary.md).
