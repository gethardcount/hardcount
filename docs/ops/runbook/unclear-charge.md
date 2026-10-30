# A charge's result is unclear

Hardcount sent a charge to Stripe and does not know yet whether the customer
paid. Until it knows, it does not charge that invoice again. One rare case
still makes a customer pay twice: Stripe finishes the first charge after
Hardcount has charged again, which takes an outage at Stripe. Hardcount then
flags the second payment (`payment.duplicate_charge`) for you to refund.
Most of these end on their own. Find your case, then
follow its Check, Fix and Verify. Back to the [runbook index](index.md).

| What you see | Case |
|---|---|
| Invoices show `payment_unconfirmed` for hours, and the invoice has a Stripe PaymentIntent | [Unconfirmed, with a PaymentIntent](#unconfirmed-with-a-paymentintent) |
| An invoice says "Stripe still hasn't confirmed this payment", its payment is `processing`, or its activity shows "Payment retry N is processing" for days | [Processing for days](#processing-for-days) |
| `hardcount_parked_invoices{disposition="open"}` above 0 | [Parked: no PaymentIntent](#parked-no-paymentintent) |
| `stale charge check: can't confirm an earlier charge with Stripe` in the logs, or `hardcount_stale_charge_hold_resolutions_total{result="cannot_ask"}` rising | [Interrupted: Hardcount cannot ask Stripe](#interrupted-hardcount-cannot-ask-stripe) |
| `hardcount_creditnote_pending_issue_drafts` growing over days | [Clawback drafts waiting](#clawback-drafts-waiting) |

Hardcount normally ends a park within a day: with no payment at Stripe 24 hours
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
SET app.bypass_rls = 'on';
SELECT id, payment_status, stripe_payment_intent_id, updated_at
FROM invoices
WHERE payment_status = 'unknown' AND stripe_payment_intent_id <> ''
  AND updated_at < now() - interval '1 hour'
LIMIT 20;
```

The reconciler's log lines start with `reconcile:`; a PaymentIntent it
skips because it is still in flight is logged at DEBUG only. To see that it
runs at all: `hardcount_reconciler_sweeps_total{reconciler="payment_unknown",outcome="run"}`
grows once per billing tick.

**Fix:**

There is no bulk or per-invoice re-check, and no dashboard action to charge
it again. Hardcount refuses to charge an invoice whose payment is `unknown`
(**Collect payment** answers 409), so it is never charged twice. The
reconciler re-checks it every tick and settles any PI that has reached a
final state at Stripe.

To end it sooner, cancel the PI in Stripe. The reconciler then records it as
failed.

**Verify:** the query above no longer returns the invoice, and the invoice
no longer shows `payment_unconfirmed`.

## Processing for days

**Symptom:** an invoice's `payment_status` is `processing`, and after 6 hours
its notice reads "Stripe still hasn't confirmed this payment after 6 hours,
so check it in Stripe." When a dunning retry made the payment, its activity
shows "Payment retry N is processing", and its dunning run stays under
Active on the Dunning page with no new retry.

**Why:** Stripe has the payment and has not finished it. Card payments end
within seconds; a payment method that settles later, such as a bank debit,
can take days. While the payment is processing, Hardcount does not charge the
invoice, retry it or run dunning's final action. Void, write-off and offline
payment are refused. The payment's webhook ends the wait. Without one, the
reconciler reads the PaymentIntent on each scheduled run once the payment is
30 minutes old, and records what Stripe said.

**Check:**

```sql
SET app.bypass_rls = 'on';
SELECT i.id, i.stripe_payment_intent_id, i.provider_payment_status, i.provider_synced_at,
       r.id AS run_id, r.state, r.attempt_count, r.next_action_at, r.awaiting_payment_intent_id
FROM invoices i
LEFT JOIN invoice_dunning_runs r ON r.invoice_id = i.id
WHERE i.payment_status = 'processing' AND i.updated_at < now() - interval '1 day'
LIMIT 20;
```

`provider_payment_status` is the PaymentIntent's status at Stripe when the
reconciler last read it (`provider_synced_at`). `awaiting_payment_intent_id`
is set when the run waits on this payment.

**Fix:**

- Stripe still shows the payment `processing`: wait. Nothing in Hardcount
  ends it sooner, and nothing charges the invoice twice meanwhile.
- Stripe shows it `succeeded` or failed, and the invoice still says
  `processing`: the webhook was lost and the reconciler has not run. Check
  it runs ([A background job is stuck](background-job-stuck.md)); its log
  lines start with `reconcile:`. It settles the payment on its next run.

**Verify:** the query no longer returns the invoice. A payment that
succeeded marks the invoice paid and closes its dunning run as recovered. One
that failed puts the invoice back to `failed`. Its run stays active with
`awaiting_payment_intent_id` empty, the customer gets that retry's reminder,
and the next retry is due at the failure plus the policy's next wait. On the
last retry the policy's final action follows instead.

## Parked: no PaymentIntent

A parked invoice is one whose last charge got an unclear answer from Stripe
(a 5xx, a 409, a network error, or a request past the caller's time limit)
that named no payment. So Hardcount cannot yet tell whether the customer paid.

**Symptom:** `hardcount_parked_invoices{disposition="open"}` above 0. The gauge
counts only parks Hardcount has not ended on schedule. Parks less than 26 hours
past their hold are not counted: Hardcount normally ends them by about 25 hours.
A park with no hold is always counted.

**Why:**

- A parked invoice is not charged while the answer is unclear, on purpose:
  no sweep, no dunning retry, no **Collect payment** and no hosted page.
  That is what keeps the customer from paying twice.
- Every billing tick (hourly), Hardcount lists the customer's payments in Stripe.
  A payment for the invoice that succeeded marks it paid; one still
  processing is followed until it ends. The charge's webhook can settle it
  sooner.
- If no payment has appeared 24 hours after the attempt, the invoice goes
  back to `pending` (`failed` after a dunning retry) and is charged again as
  usual. Stripe can still create a payment that long after a 5xx, while it
  recovers from an incident, so Hardcount waits that long before charging again.
- So a counted park is one the check could not finish. Either Hardcount cannot
  ask Stripe about it (see [Interrupted](#interrupted-hardcount-cannot-ask-stripe)),
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
   deleted in Stripe, so Hardcount cannot ask), send its
   `payment_intent.succeeded` event again from the Stripe dashboard to the
   Hardcount webhook endpoint. The webhook marks the invoice paid, a written-off
   one too.
2. If nothing was charged and the park does not end, or the customer paid
   another way, mark the invoice uncollectible. It is the one action the
   invoice page allows on a parked invoice. In the park's first few minutes,
   while the charge's claim is still live, it is refused ("being confirmed
   with Stripe"); try again a few minutes later. It closes its dunning run. A
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
uncollectible before the 24 hours end, or Hardcount charges the card again.

**Verify:** `hardcount_parked_invoices{disposition="open"}` falls back to zero.

## Interrupted: Hardcount cannot ask Stripe

A charge is interrupted when Hardcount sent it to Stripe but did not save the
answer (a crash, a shutdown, a closed **Collect payment**, or a 429 on a
resend). Hardcount holds the invoice and asks Stripe before charging again
([ADR-127](../../adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)).
This section is for when it cannot ask.

**Symptom:**

- This ERROR in the logs, every hour for the same invoice:
  `stale charge check: can't confirm an earlier charge with Stripe; write the invoice off or fix the Stripe connection`.
- `hardcount_stale_charge_hold_resolutions_total{result="cannot_ask"}` rising.
- An invoice stays Pending or Failed. **Collect payment**, void and
  offline payment answer: "A charge on this invoice is being confirmed with
  Stripe." Dunning does not retry it, and its final action waits.

A steady `hardcount_stale_charge_hold_resolutions_total{result="failed"}` is
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

Until it can ask, Hardcount will not charge the invoice again: the interrupted
charge may have taken the money.

**Check:**

```sql
SET app.bypass_rls = 'on';
-- Invoices whose charge hold ended more than 10 minutes ago
SELECT i.id, i.tenant_id, i.invoice_number, i.status, i.payment_status,
       i.auto_charge_claimed_until, i.provider_synced_at, c.stripe_customer_id
FROM invoices i
LEFT JOIN customers c ON c.id = i.customer_id
WHERE i.auto_charge_claimed_until < now() - interval '10 minutes'
ORDER BY i.auto_charge_claimed_until;
```

In the Stripe dashboard, open the customer (`stripe_customer_id` above) and
look for a payment whose metadata `hardcount_invoice_id` is the invoice's id.

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
- `hardcount_stale_charge_hold_resolutions_total` shows `settled`, `released` or
  `cleared` for the run that resolved it.

## Clawback drafts waiting

A clawback is a credit note Hardcount issues when a paid, in-advance period is
cut short, for example by a downgrade. Hardcount writes it as a draft first, and
a draft whose source payment is still unclear waits for it.

**Symptom:** `hardcount_creditnote_pending_issue_drafts` grows over days.

**Check and fix:** is `hardcount_parked_invoices{disposition="open"}` above
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
