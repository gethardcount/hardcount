# Manual fixes

Operator actions for incidents. Where a fix below writes to the database
directly, record it in the audit log yourself. Back to the [runbook index](index.md).

## Force-resolve a stuck dunning run

A dunning run is Velox's schedule of retries for one unpaid invoice. Use
this only when the dashboard **Resolve** action is unavailable.

1. **Pick the resolution that says what actually happened to the invoice.**
   Finance later reads this column to answer "why did we stop collecting
   this?", and this SQL skips the endpoint that would otherwise check it.

   | Invoice ended up | Use |
   |---|---|
   | paid outside Velox | `payment_recovered` |
   | annulled or billed in error | `invoice_voided` |
   | written off as bad debt | `invoice_not_collectible` |

2. **Resolve the run:**

   ```sql
   UPDATE invoice_dunning_runs
   SET state = 'resolved', resolution = 'invoice_voided',  -- see table above
       resolved_at = now(), next_action_at = NULL
   WHERE id = '<run_id>';

   INSERT INTO invoice_dunning_events (tenant_id, run_id, invoice_id, event_type, state, reason)
   VALUES ('<tenant_id>', '<run_id>', '<invoice_id>', 'resolved', 'resolved', 'invoice_voided');
   ```

3. **Change the invoice too.** This SQL resolves the run only; it does not
   touch the invoice. The endpoint makes the matching invoice change (void,
   mark uncollectible, or record a payment), and this SQL does not. If you
   skip this step, the run and the invoice disagree.

## Mark an invoice paid (offline payment received)

Use **Record offline payment** in the invoice's ⋯ menu (on a written-off
invoice, the **Record payment** button), or
`POST /v1/invoices/{id}/record-payment` (optional body
`{"note": "Cheque #1234"}`). It also closes the dunning run, sends
`invoice.paid` and writes the audit row, which an SQL update would skip.
Do not mark an invoice paid with SQL. It is refused while a charge on the
invoice is still being confirmed with Stripe: see
[A charge's result is unclear](unclear-charge.md).

## A subscription change is refused with `unpaid_invoice_blocks_change`

A plan change, added item or seat increase would charge for the rest of
the current period, and the invoice that billed that period still has an
amount due. A written-off invoice counts as unpaid. Do one of:

- record the payment, if the customer paid outside Stripe (above);
- void the invoice, if it should not be owed. A void hands back the credits
  applied to it and reverses its tax;
- for a plan change, schedule it for the end of the period
  (`immediate: false`). Nothing is charged for the rest of this one.

After a payment or a void, make the change again. A cancel, downgrade or
removed item is never refused for this: it lowers what that invoice asks
for by the unused time. A cancel on the period's first day voids the invoice instead, if nothing
is paid on it, no charge on it is being confirmed with Stripe and it bills
nothing else.

If the customer paid a new invoice you made to recover a written-off one,
Velox does not link the two. The written-off invoice keeps its amount due,
so the change is still refused, and a later cancel or downgrade lowers what
it asks for instead of crediting the customer. Do not record a payment on
it: the money would be counted twice. Schedule a plan change for the
period's end. For a cancel or downgrade, grant the customer the unused time
as credit (`POST /v1/credits/grant`). An added item or seat increase has no
way round yet.

## A customer has no usable payment method on file

Saved cards live in the
`payment_methods` table, written by the Stripe `setup_intent.succeeded` and
`payment_method.attached` webhooks. If a webhook was missed, do not change
the data with SQL. Instead:

- send the webhook again from the Stripe dashboard, or
- ask the customer to add a card again on the hosted payment-setup page.
