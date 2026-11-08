# Usage below zero

Hardcount accepts negative usage events as corrections. When the corrections in
a billing period are larger than the usage they offset, that meter's usage
for the period is below zero. Back to the [runbook index](index.md).

## Symptom

- The subscription's activity timeline shows **Usage below zero billed as
  zero**, with each meter and its amount, for example `api_calls -50`.
- Or the server logs a WARN `cycle: usage below zero after corrections —
  billed as 0` with `meter`, `quantity` and `subscription_id`. The prefix is
  `cycle`, `cancel` or `threshold`.
- The invoice preview and the customer usage view warn that the meter's
  usage is below zero and counted as 0.

## Why

A negative quantity has no price, so Hardcount counts that meter as 0 for the
period and builds the rest of the invoice: base fees and every other meter.
It does not carry the negative amount into a later period.

- Hardcount writes one activity row for each invoice built this way: the
  invoice at the period's end, a cancel's final invoice, or a
  [spend-threshold invoice](../../concepts/invoices.md#spend-thresholds-an-invoice-made-early).
  A period end with nothing else to bill makes no invoice and still writes
  the row.
- The rule applies to each price on its own. A meter with several prices,
  such as a tokens meter with one price per model, can bill one model and
  count another as 0. Another meter's usage is not reduced.
- A common cause is a correction for usage that an earlier invoice already
  billed. The correction then lands in a later period, where there is less
  usage to offset.

## Check

1. **Read the row's details.** It is the audit log entry with action
   `subscription.net_negative_usage`:

   ```bash
   SUB=<subscription id>
   curl -sS "$API/v1/audit-log?action=subscription.net_negative_usage&resource_id=$SUB" \
     -H "Authorization: Bearer $KEY" | jq '.data[] | .metadata'
   ```

   **You see:** `meters` (each with `meter_id`, `event_name`, `meter_name`,
   `quantity`, `from` and `to`), `billing_reason`, `period_start` and
   `period_end`, and `invoice_id` and `invoice_number` when an invoice was
   made.

2. **List the meter's events in its window.** Use the meter's `from` and
   `to`, not the period. They differ when a spend-threshold invoice already
   billed the start of the period, or when the meter was on the
   subscription for only part of it:

   ```bash
   CUSTOMER=<customer id>; METER=<meter_id>; FROM=<from>; TO=<to>
   curl -sS -G "$API/v1/usage-events" -H "Authorization: Bearer $KEY" \
     --data-urlencode "customer_id=$CUSTOMER" --data-urlencode "meter_id=$METER" \
     --data-urlencode "from=$FROM" --data-urlencode "to=$TO" \
     | jq -c '.data[] | {quantity, dimensions, timestamp, idempotency_key}'
   ```

   For a meter with several prices, `quantity` in the row adds up only the
   prices whose usage was below zero. Look at the events whose labels those
   prices take, such as `"model": "claude-sonnet-4.5"`.

3. **Find the corrections** (negative `quantity`) **and the usage they
   correct.** That usage may be in an earlier period.

## Fix

- **The corrected usage was billed on an earlier invoice:** create a credit
  note on that invoice for the corrected amount
  (`POST /v1/credit-notes` with `invoice_id` and `lines`), then issue it
  (`POST /v1/credit-notes/{id}/issue`). With no `refund_amount_cents`, the
  credit goes to the customer's balance and no money moves
  ([Credit notes](../../concepts/invoices.md#credit-notes)). Before you set
  a refund, check the payment in Stripe: a refund made there already is
  recorded instead ([A refund made in Stripe](refund-made-in-stripe.md)).
- **The correction was sent by mistake:** nothing was charged for it. Fix
  the sender so it does not repeat.

## Verify

The credit note is issued on the earlier invoice, and the customer's
balance or refund shows it.

Related: [Invoice timing and late usage](invoice-timing-late-usage.md).
