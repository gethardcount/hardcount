# Manual fixes

Operator actions for incidents. A fix below that writes to the database
directly writes no audit log entry, and Hardcount has no way to add one by
hand: keep your own record of what you ran, when and why. Back to the [runbook index](index.md).

## Force-resolve a stuck dunning run

A dunning run is Hardcount's schedule of retries for one unpaid invoice.
Resolve it on the run's invoice in the dashboard (the Dunning page's
**Open invoice**): **Record offline payment**, **Void invoice** or **Mark
uncollectible** each close the run. Or call
`POST /v1/dunning/runs/{id}/resolve` with a `resolution` (one of the three
below). Both make the matching change to the invoice and send
`dunning.resolved`. Use the SQL below only when neither is available.

1. **Pick the resolution that says what actually happened to the invoice.**
   Finance later reads this column to answer "why did we stop collecting
   this?", and this SQL skips the endpoint that would otherwise check it.

   | Invoice ended up | Use |
   |---|---|
   | paid outside Hardcount (check Stripe first: [Mark an invoice paid](#mark-an-invoice-paid-offline-payment-received)) | `payment_recovered` |
   | annulled or billed in error | `invoice_voided` |
   | written off as bad debt | `invoice_not_collectible` |

2. **Find the run's account, invoice and mode.** Step 3 needs all three.
   For `livemode`, `true` is a live run, `false` a test run:

   ```sql
   SET app.bypass_rls = 'on';
   SELECT tenant_id, invoice_id, livemode FROM invoice_dunning_runs WHERE id = '<run_id>';
   ```

3. **Resolve the run.** Set `app.livemode` to `'on'` for a live run or
   `'off'` for a test run. The event row takes its mode from this setting,
   and the insert is refused without it. The example uses `invoice_voided`:
   replace it with the resolution from step 1 in both places, the UPDATE and
   the INSERT.

   ```sql
   BEGIN;
   SET LOCAL app.bypass_rls = 'on';
   SET LOCAL app.livemode = 'on';  -- 'off' for a test run

   UPDATE invoice_dunning_runs
   SET state = 'resolved', resolution = 'invoice_voided',  -- see table above
       resolved_at = now(), next_action_at = NULL
   WHERE id = '<run_id>';

   INSERT INTO invoice_dunning_events (tenant_id, run_id, invoice_id, event_type, state, reason)
   VALUES ('<tenant_id>', '<run_id>', '<invoice_id>', 'resolved', 'resolved', 'invoice_voided');
   COMMIT;
   ```

4. **Change the invoice too.** This SQL resolves the run only; it does not
   touch the invoice. The endpoint makes the matching invoice change (void,
   mark uncollectible, or record a payment), and this SQL does not. If you
   skip this step, the run and the invoice disagree. No safe SQL makes this
   change: make it from the dashboard or the API once one is back
   (**Void invoice**, **Mark uncollectible** or **Record offline payment**
   on the invoice).

## Mark an invoice paid (offline payment received)

1. **Check that Stripe has no payment for the invoice.** Every payment
   Hardcount starts carries the invoice's ID as `hardcount_invoice_id`
   metadata. Use the secret key of the Stripe account connected in the
   invoice's mode:

   ```bash
   curl -sS -G https://api.stripe.com/v1/payment_intents/search -u "$STRIPE_SECRET_KEY:" \
     --data-urlencode "query=metadata['hardcount_invoice_id']:'<invoice id>'" \
     | jq '.data[] | {id, status, amount, currency, created}'
   ```

   - **Nothing, or only `requires_payment_method` or `canceled`:** no money
     moved through Stripe. Go on to step 2. Stripe's search can take up to
     a minute to show a new payment.
   - **`succeeded`:** the customer paid through Stripe, and Hardcount has
     not recorded it yet. Do not record a payment: the money would be
     counted twice. See [A charge's result is unclear](unclear-charge.md).
   - **`processing`, `requires_action`, `requires_confirmation` or
     `requires_capture`:** the payment can still go through. Wait until it
     ends.

   Stripe's search is not offered to Stripe accounts in India. There,
   list the customer's payments instead, with the customer's
   `stripe_customer_id` from `GET /v1/customers/{id}`, and read the
   statuses the same way:

   ```bash
   curl -sS -G https://api.stripe.com/v1/payment_intents -u "$STRIPE_SECRET_KEY:" \
     --data-urlencode "customer=<stripe_customer_id>" --data-urlencode "limit=100" \
     | jq '.data[] | select(.metadata.hardcount_invoice_id == "<invoice id>") | {id, status, amount, currency, created}'
   ```

2. **Record the payment.** Use **Record offline payment** in the invoice's
   ⋯ menu (on a written-off invoice, the **Record payment** button), or
   `POST /v1/invoices/{id}/record-payment` (optional body
   `{"note": "Cheque #1234"}`). It also closes the dunning run, sends
   `invoice.paid` and writes the audit row. Record payment is refused while
   a charge on the invoice is still being confirmed with Stripe: see
   [A charge's result is unclear](unclear-charge.md). Do not mark an
   invoice paid with SQL: an SQL update skips all of this.

## A subscription change is refused with `unpaid_invoice_blocks_change`

The invoice that billed the current period still has an amount due. A
written-off invoice counts as unpaid.

| Change | What happens |
|---|---|
| Plan change, added item or seat increase | Refused: it would charge for the rest of the period on top of the unpaid invoice. |
| Cancel, downgrade or removed item | Never refused. It lowers what that invoice asks for by the unused time. |
| Cancel on the period's first day | Voids the invoice instead, if nothing is paid on it, no charge on it is being confirmed with Stripe, and it bills nothing else. |

For a refused change, do one of:

- record the payment, if the customer paid outside Stripe
  ([above](#mark-an-invoice-paid-offline-payment-received));
- void the invoice, if it should not be owed. A void hands back the credits
  applied to it and reverses its tax;
- for a plan change, schedule it for the end of the period
  (`immediate: false`). Nothing is charged for the rest of this one.

After a payment or a void, make the change again.

### If you re-invoiced a written-off invoice

Hardcount does not link a new invoice you made to recover a written-off
one. The written-off invoice keeps its amount due, so the change is still
refused. Do not record a payment on it: the money would be counted twice.

- Schedule a plan change for the period's end.
- A cancel or downgrade lowers what the written-off invoice asks for,
  instead of crediting the customer. Grant the customer the unused time as
  credit (`POST /v1/credits/grant`).
- An added item or seat increase has no way round yet.

## A customer has no usable payment method on file

Saved cards are stored in the `payment_methods` table. Hardcount writes it when
Stripe's `setup_intent.succeeded` webhook arrives, or `checkout.session.completed`
for a card saved on a hosted page. If a webhook was missed, do not change
the data with SQL. Instead:

- send the webhook again from the Stripe dashboard, or
- ask the customer to add a card again on the hosted payment-setup page.

## Skip the emails and webhooks queued before a restore

After a restore, Hardcount sends every `pending` row of `email_outbox` and
`webhook_outbox` when its jobs resume. Rows the old database had already sent
after the backup go out a second time. To skip the rows queued before you
started the restored Hardcount, mark them sent, while the `email_outbox` and
`webhook_outbox` jobs are still paused
([Restore Hardcount](../restore.md#queued-emails-and-webhooks)). Put the time
you noted just before that start in both lines. The rows queued after it stay
`pending`: they are the receipts and webhooks of the payments recorded while
matching Hardcount with Stripe.

```sql
SET app.bypass_rls = 'on';
UPDATE email_outbox SET status = 'dispatched', dispatched_at = now(),
  payload = payload - '{setup_url,update_url,password_reset_url,invite_url,public_token,pdf}'::text[]
  WHERE status = 'pending' AND created_at < '2026-10-20 09:15:00+00';
UPDATE webhook_outbox SET status = 'dispatched', dispatched_at = now()
  WHERE status = 'pending' AND created_at < '2026-10-20 09:15:00+00';
```

**You see:** `SET`, then `UPDATE <n>` twice. The `payload - '{…}'` part
removes the links and the PDF from the email rows, so they are not kept for
emails that were never sent.
