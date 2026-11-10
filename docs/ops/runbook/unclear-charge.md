# A charge's result is unclear

This page is for the operator who sees a card charge whose result Hardcount
does not know yet. Until Hardcount knows, it does not charge that invoice
again, and most cases end on their own within about an hour. Find your case
in the table, then follow its Check, Fix and Verify. Back to the
[runbook index](index.md).

| What you see | Case |
|---|---|
| Invoices show `payment_unconfirmed` for hours, and the invoice has a Stripe PaymentIntent | [Unconfirmed, with a PaymentIntent](#unconfirmed-with-a-paymentintent) |
| `reconciler error` in the logs with `get payment intent`, on every billing run for the same invoice | [Unconfirmed, with a PaymentIntent](#unconfirmed-with-a-paymentintent) |
| An invoice says "Stripe still hasn't confirmed this payment", its payment is `processing`, or its activity shows "Payment retry N is processing" for days | [Processing for days](#processing-for-days) |
| An invoice stays Pending or Failed for more than two hours and refuses **Collect payment** with "A charge on this invoice is being confirmed with Stripe", or `hardcount_stale_charge_hold_resolutions_total{result="failed"}` rising | [A charge hold that does not clear](#a-charge-hold-that-does-not-clear) |
| `stale charge check: this charge hold or parked charge was left by an earlier Hardcount version` in the logs, `hardcount_stale_charge_hold_resolutions_total{result="earlier"}` rising, or `hardcount_parked_invoices{disposition="open"}` above 0 | [Left by an earlier version](#left-by-an-earlier-version) |
| `hardcount_creditnote_pending_issue_drafts` growing over days | [Clawback drafts waiting](#clawback-drafts-waiting) |
| Incomplete payments in the Stripe dashboard that Hardcount never confirmed | [How a charge reaches Stripe](#how-a-charge-reaches-stripe) |

## How a charge reaches Stripe

Every charge Hardcount makes without the customer present takes three steps.
That is the automatic charge, a dunning retry (Hardcount's retrying of a
failed card payment) and **Collect payment**
([ADR-137](../../adr/137-charge-stamps-invoice-before-confirming.md)).

1. Create the Stripe PaymentIntent (PI), unconfirmed. No money moves.
2. Put it on the invoice: the invoice's `payment_status` becomes `unknown`
   on that PI, and the attempt is recorded. This write applies only while
   the charge still holds the invoice, the invoice is still finalized and
   `pending` or `failed`, and its amount due has not changed. The charge
   hold is a 5-minute claim that lets only one charge run on an invoice at
   a time (`auto_charge_claimed_until`).
3. Confirm the PI. Money moves.

Hardcount confirms only a PI that step 2 put on the invoice. So for the
second or so that a charge takes, the invoice shows "Confirming payment"
(`payment_unconfirmed`). Then it is paid, failed or processing.

While the invoice is `unknown`, Hardcount refuses a void, an offline
payment, a credit note, a second charge and **Pay** on the hosted invoice
page. A write-off is allowed once no charge holds the invoice.

Where a charge stops decides what it leaves:

- **Before step 2:** nothing was confirmed, so the charge moved no money,
  and the invoice was never `unknown`.
  - A crash, or an unclear answer to the create (a 5xx, a 409, a timeout or
    a network error), leaves the invoice `pending` or `failed` with its
    charge hold set. The reconciler clears the hold on the first billing run
    at least 10 minutes after the hold ends. It asks Stripe nothing, and the
    next charge uses a new idempotency key
    ([A charge hold that does not clear](#a-charge-hold-that-does-not-clear)).
  - Any other failure frees the charge hold at once: unreadable Stripe
    credentials, a 429, a refused key, or step 2 not applying because the
    invoice changed or was paid meanwhile.
- **After step 2, with no definite answer to the confirm:** the invoice
  already says `unknown` on that PI, so nothing more needs saving. That covers
  a crash, a timeout, a 5xx, a 409, a 429, a key Stripe refuses and a
  closed **Collect payment**. The charge hold stays until it ends, at most
  5 minutes after the charge began. Then the reconciler reads the PI by its
  ID and records what Stripe says
  ([Unconfirmed, with a PaymentIntent](#unconfirmed-with-a-paymentintent)).
- **A decline at step 3** is recorded at once, as for any declined charge,
  and the charge hold is freed.

The reconciler is the check that asks Stripe what happened to a payment
Hardcount has no answer for. It also clears a charge hold that a stopped
charge left behind. It runs with every scheduled billing run: hourly, or
every 5 minutes when `APP_ENV` is `local` or unset.

A PI that a charge created and never put on an invoice can stay in the
Stripe dashboard as incomplete (status `requires_confirmation`). It takes no
money: Hardcount confirms only a PI that is on the invoice.

One case can still make a customer pay twice: the same invoice is paid
through two channels. For example, you record a bank transfer as an offline
payment while the customer also completes a payment in Stripe. Hardcount
flags a second payment it learns about (`payment.duplicate_charge`) for you
to refund in Stripe ([ADR-068](../../adr/068-checkout-session-dedup.md)).
A charge Hardcount itself makes cannot be that second channel: while it is
at Stripe the invoice is `unknown`, and an offline payment is refused.

## Unconfirmed, with a PaymentIntent

**Symptom:** invoices show the `payment_unconfirmed` attention reason
("Confirming payment") for hours. Their `payment_status` is `unknown`, and
they have a `stripe_payment_intent_id`. Every charge shows this for the
second or so it takes; that needs no action.

**What Hardcount does:** the PI's webhook from Stripe can settle the payment
at once. Otherwise the reconciler reads the PI by its ID on each billing
run. It waits until no charge holds the invoice any more, which is 5 minutes
after the charge began, and until the invoice's last change is a minute old.

| The PI at Stripe | What the reconciler does |
|---|---|
| `succeeded` | Records the invoice paid, also when the payment was refunded at Stripe since (below the table). |
| `processing` | Records what it saw. The invoice stays `unknown`, and the reconciler reads the PI again on each run until it ends. |
| `requires_confirmation`, `requires_action` or `requires_capture`: the payment never finished | Cancels the PI at Stripe, then reads it again. With no charge and no payment error on it, no bank answered, and the attempt ends with no payment (below the table). A decline that landed before the cancel is recorded as a failed payment, and so is a payment that needed the customer to authenticate. |
| `canceled`, with no charge and no payment error | The attempt ends with no payment (below the table). |
| `requires_payment_method` (declined), or `canceled` after a charge or a payment error | Records a failed payment, as for any declined charge. |

**An attempt that ends with no payment.** A canceled PI is final: Stripe
refuses a confirm that arrives later. The invoice goes back to `pending`
(`failed` after a dunning retry), with no payment-failed email. It is
charged again as usual, under a new idempotency key. An automatic charge can
go out in the same billing run.

**A payment refunded at Stripe before Hardcount read it** is still that
payment: the invoice is marked paid, and the refund shows on it as
[a refund made in Stripe](refund-made-in-stripe.md) once Stripe's refund
event arrives.

**Why it lasts hours:**

- The reconciler is not running. See
  [A background job is stuck](background-job-stuck.md).
- Stripe cannot be read: an outage, Stripe disconnected for this mode, a
  rejected key, or a PI that is not in the connected Stripe account. The
  invoice stays `unknown`, and nothing charges it.
- The PI is still `processing` at Stripe. The reconciler waits for it to
  end.
- Stripe refused the cancel of a PI that never finished. Either a confirm
  got there first, and the next run reads what it did, or the account's key
  may read PaymentIntents but not cancel them.

**Check:**

```sql
SET app.bypass_rls = 'on';
SELECT id, payment_status, stripe_payment_intent_id, provider_payment_status,
       provider_synced_at, auto_charge_claimed_until, updated_at
FROM invoices
WHERE payment_status = 'unknown' AND stripe_payment_intent_id <> ''
  AND updated_at < now() - interval '1 hour'
LIMIT 20;
```

`provider_payment_status` is the PI's status at Stripe when the reconciler
last read it, and `provider_synced_at` is when it last tried. The
reconciler's own log lines start with `reconcile:`; a PI it skips because it
is still processing is logged at DEBUG only. A PI it cannot read logs
`reconciler error` on each run, with
`reconciler` `payment_unknown` and an `error` that starts
`invoice <id>: get payment intent <PI>`. A cancel Stripe refuses logs the
same line with `cancel unfinished payment intent <PI>`. To see that the
reconciler runs at all:
`hardcount_reconciler_sweeps_total{reconciler="payment_unknown",outcome="run"}`
grows once per billing tick.

**Fix:**

There is no bulk or per-invoice re-check, and no dashboard action to charge
it again. Hardcount refuses to charge an invoice whose payment is `unknown`
(**Collect payment** answers 409), so it is never charged twice.

1. **Stripe cannot be read for now:** fix the connection under Settings →
   Payments if it is broken. The next billing run settles the PI.
2. **Stripe refuses the cancel on every run:** cancel the PI in the Stripe
   dashboard, or give the account's key permission to write PaymentIntents.
   The next run records that no money was taken, and the invoice is charged
   again as usual.
3. **Stripe can never show the PI** (the key is revoked for good, or the
   account now uses another Stripe account), or the customer paid another
   way: mark the invoice uncollectible.
   - The write-off is allowed once no charge holds the invoice, 5 minutes
     after the charge began. Until then it is refused with "A charge on
     this invoice is being confirmed with Stripe"; try again a few minutes
     later.
   - It moves no money. A payment Stripe took still settles the invoice
     later: it moves from uncollectible to paid.
   - It closes the invoice's dunning run. A clawback draft held on the
     invoice still waits while its payment is `unknown`.
   - When Stripe can never show the PI, the reconciler still tries to read
     it on each run, and logs a `reconciler error` line for it each time.
4. A write-off closes the invoice's dunning run without the policy's final
   action (cancel or pause the subscription). Check whether that action was
   due:
   - Find the run with `GET /v1/dunning/runs?invoice_id=<invoice id>`, then
     the policy it runs on with `GET /v1/dunning/policies/<policy_id>`. A run
     keeps the policy it started on, even if the customer moved to another.
   - If the run's `attempt_count` has reached its `max_retry_attempts` (the
     number of entries in the policy's `retry_days`) and the policy's
     `final_subscription_action` is `cancel` or `pause`, apply that action by
     hand on the subscription page. Dunning held it because a payment was in
     flight.
   - With no run, or with retries left, the final action was never due. The
     charge went out before dunning started or before its last retry.

**Verify:** the query above no longer returns the invoice, and the invoice
no longer shows `payment_unconfirmed`.

## Processing for days

**Symptom:** an invoice's `payment_status` is `processing`, and after 6 hours
its notice reads "Stripe still hasn't confirmed this payment after 6 hours,
so check it in Stripe." Dunning is Hardcount's retrying of a failed card
payment, and a dunning run is the retries for one invoice. When a dunning
retry made the payment, its activity shows "Payment retry N is processing",
and its dunning run stays under Active on the Dunning page with no new retry.

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

<!-- The anchor below keeps older links to this page's former "Interrupted" section working. -->
<a id="interrupted-hardcount-cannot-ask-stripe"></a>

## A charge hold that does not clear

A charge that stops before it puts its PI on the invoice (step 2 in
[How a charge reaches Stripe](#how-a-charge-reaches-stripe)) can leave the
invoice `pending` or `failed` with its charge hold set. No confirm was sent,
so nothing was charged. On the first billing run at least 10 minutes after
the hold ends, the reconciler clears the hold without asking Stripe, and the
next charge uses a new idempotency key. With the hourly run, that is within
about an hour and a quarter of the charge. This section is for a hold that
stays longer.

**Symptom:**

- An invoice stays Pending or Failed for more than two hours. **Collect
  payment**, void and offline payment answer: "A charge on this invoice is
  being confirmed with Stripe." Dunning does not retry it, and its final
  action waits.
- `hardcount_stale_charge_hold_resolutions_total{result="failed"}` rising.

**Why:** the reconciler counts each hold it checks in
`hardcount_stale_charge_hold_resolutions_total`, by `result`:

| `result` | What happened |
|---|---|
| `released` | The hold was on an invoice still `pending` or `failed`: its charge never confirmed. The hold is cleared and the attempt counter moved, so the invoice can be charged again. |
| `cleared` | A leftover hold on an invoice that is `unknown`, `processing`, paid, voided or written off. The hold is cleared, and nothing else changes. |
| `failed` | The database write failed, or the check panicked. The hold stays for a later run. |
| `earlier` | An earlier version left the hold, or a parked charge. Hardcount keeps it for a person: [Left by an earlier version](#left-by-an-earlier-version). |
| `lost_race` | Another write changed the invoice first. The next run looks again. |

So a hold stays when the reconciler is not running, when its write keeps
failing, or when an earlier version left it. The reconciler never asks
Stripe about a hold. A payment it cannot read at Stripe is an `unknown`
invoice: see [Unconfirmed, with a PaymentIntent](#unconfirmed-with-a-paymentintent).

**Check:**

```sql
SET app.bypass_rls = 'on';
-- Invoices with a charge hold, oldest first
SELECT id, tenant_id, invoice_number, status, payment_status,
       auto_charge_claimed_until, charge_hold_token, provider_synced_at,
       stripe_payment_intent_id
FROM invoices
WHERE auto_charge_claimed_until IS NOT NULL
ORDER BY auto_charge_claimed_until;
```

Read each row by its hold (`auto_charge_claimed_until`):

- **In the future:** the hold was taken less than 5 minutes ago, by a charge
  or by dunning's final action. A charge is running, or it stopped without
  a definite answer.
- **Ended less than 10 minutes ago:** the reconciler has not looked yet.
- **Ended earlier, with `charge_hold_token` equal to it:** the reconciler
  clears it on its next run, once `provider_synced_at` is empty or at least
  30 minutes old.
- **Ended earlier, with `charge_hold_token` empty or different:** an earlier
  version took it. See [Left by an earlier version](#left-by-an-earlier-version).

A `failed` check logs `reconciler error` with `reconciler` `payment_unknown`
and an `error` that names the invoice and `clear stale hold`. A check that
panicked logs `step panicked; running the rest` with `step`
`stale_charge_check`.

**Fix:**

1. **The reconciler is not running:** see
   [A background job is stuck](background-job-stuck.md).
2. **`failed`:** fix the database error the log line names. The next run
   clears the hold on its own.
3. **`earlier`:** follow
   [Left by an earlier version](#left-by-an-earlier-version).

Do not clear `auto_charge_claimed_until` (the charge hold) by hand on an open
invoice. The reconciler's clear also moves the attempt counter, so the next
charge sends a new idempotency key. And a hold that an earlier version left
may cover a charge that took money.

**Verify:**

- The query above no longer lists the invoice.
- `hardcount_stale_charge_hold_resolutions_total` shows `released` or
  `cleared` for the run that cleared it.

## Left by an earlier version

Earlier Hardcount versions charged in one call, and saved nothing before the
charge reached Stripe. Such a charge could leave an invoice with a charge
hold, or parked (next section). The upgrade to this version refuses to run
while any exists
([Upgrade Hardcount](../../upgrades.md#migration-00225-finish-charges-first)).
So you see this case only when a copy of an earlier version ran against the
upgraded database.

**Symptom:**

- This ERROR in the logs, every hour for the same invoice:
  `stale charge check: this charge hold or parked charge was left by an earlier Hardcount version, which charged in one call, so Hardcount cannot tell whether it took money`.
- `hardcount_stale_charge_hold_resolutions_total{result="earlier"}` rising.
- `hardcount_parked_invoices{disposition="open"}` above 0. Otherwise this
  gauge stays at 0, because this version parks no invoice.

**Why:** Hardcount's records cannot say whether that charge took money. It
does not guess: it keeps the charge hold, charges nothing on the invoice, and
logs the ERROR on each run for a person to settle the invoice.

**Check:**

1. Stop every copy of the earlier version.
2. Find the invoice's payments in Stripe by the invoice's ID, as in step 1
   of
   [Mark an invoice paid](manual-interventions.md#mark-an-invoice-paid-offline-payment-received).

**Fix:** act on what Stripe shows.

- **`succeeded`:** send its `payment_intent.succeeded` event again from the
  Stripe dashboard to the Hardcount webhook endpoint. The webhook marks the
  invoice paid, a written-off one too. The next run clears the charge hold.
- **`processing`:** wait until Stripe finishes it, then act on what it
  became.
- **Nothing, or only `requires_payment_method` or `canceled`:** mark the
  invoice uncollectible. The write-off is allowed while the charge hold
  waits, and a payment Stripe completes later still marks the invoice paid.
  The next run clears the charge hold, on a parked invoice too. If its
  dunning run used every retry, check its final action as in step 4 of
  [Unconfirmed](#unconfirmed-with-a-paymentintent).

**Verify:** the ERROR stops for the invoice, and
`hardcount_parked_invoices{disposition="open"}` is back to 0.

### Parked: no PaymentIntent

A parked invoice is one an earlier version left with its payment `unknown`
and no PI ID: its charge got an unclear answer from Stripe that named no
payment. This version never parks an invoice. A parked invoice from an
earlier version is not charged again. Void, offline payment, a credit note,
**Collect payment** and the invoice emails are refused on it with
`payment_unidentifiable`; only a write-off is allowed.

After you write it off (previous section), the next run clears its charge
hold, and the hourly ERROR stops. Nothing charges a written-off invoice, and
a payment Stripe completes later is still recorded by its webhook. The
invoice stays parked and keeps its notice, which asks you to check the
customer's payments in Stripe. A credit note draft that waits on its payment
([Clawback drafts waiting](#clawback-drafts-waiting)) keeps waiting: nothing
in this version ends a park.

**Verify:** the ERROR does not come back for the invoice on the next run,
and its `auto_charge_claimed_until` is empty.

## Clawback drafts waiting

A clawback is a credit note Hardcount issues when a paid, in-advance period is
cut short, for example by a downgrade. Hardcount writes it as a draft first, and
a draft whose source payment is still unclear waits for it.

**Symptom:** `hardcount_creditnote_pending_issue_drafts` grows over days.

**Check and fix:** each draft waits while its invoice's payment is
`processing` or `unknown`, and issues once that payment ends. An `unknown`
payment older than an hour: follow
[Unconfirmed](#unconfirmed-with-a-paymentintent). A `processing` one older
than a day: follow [Processing for days](#processing-for-days). Drafts behind
younger payments need no action.

**Verify:** the drafts drain on the next reconciler pass after their
payments end.

Related: [card payment failure handling](../../payments.md),
[ADR-137](../../adr/137-charge-stamps-invoice-before-confirming.md),
[ADR-127](../../adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md),
[ADR-107](../../adr/107-unknown-is-terminal-until-a-human.md),
[ADR-059](../../adr/059-guard-invoice-mutations-while-payment-in-flight.md)
and the [glossary](../../glossary.md).
