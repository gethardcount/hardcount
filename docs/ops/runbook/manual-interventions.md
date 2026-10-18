# Manual fixes

Operator actions for incidents, when the dashboard action is not available.
Each one writes to the database directly, so record it in the audit log
yourself. Back to the [runbook index](index.md).

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

   Do not write `manually_resolved`. It is the old value from before
   migration 0170, and meant "voided or written off". The CHECK constraint
   keeps it legal only so that older rows stay readable.

3. **Change the invoice too.** This SQL resolves the run only; it does not
   touch the invoice. The endpoint makes the matching invoice change (void,
   mark uncollectible, or record a payment), and this SQL does not. If you
   skip this step, the run and the invoice disagree.

4. **Record the action in the audit log.**

## Force-mark an invoice paid (offline payment received)

Use the dashboard's **Mark as paid** action. The SQL alternative:

```sql
-- payment_status has a CHECK (pending/processing/succeeded/failed/unknown);
-- 'paid' is an INVOICE status, not a payment_status. Mirror what MarkPaid does:
UPDATE invoices
SET status = 'paid', payment_status = 'succeeded',
    amount_paid_cents = amount_due_cents, amount_due_cents = 0,
    paid_at = now(), auto_charge_pending = false, updated_at = now()
WHERE id = '<invoice_id>';
```

Record the action in the audit log yourself if you run SQL directly.

## A customer has no usable payment method on file

There is no `setup_status` flag to change: the `customer_payment_setups`
table was dropped (migration 0097). Saved cards live in the
`payment_methods` table, written by the Stripe `setup_intent.succeeded` and
`payment_method.attached` webhooks. If a webhook was missed, do not change
the data with SQL. Instead:

- send the webhook again from the Stripe dashboard, or
- ask the customer to add a card again on the hosted payment-setup page.
