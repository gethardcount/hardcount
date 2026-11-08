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
- Or [the late-usage steps](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice)
  found a price whose usage is now lower than its invoices billed: a
  correction arrived after the invoice. On this page, that quantity below
  0 takes the place of the row's `quantity`, and the late-usage `FROM` and
  `TO` take the place of its window. The period's invoices are the ones
  that billed the corrected usage.

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
   made. A row written before 2026-10-27 has `meter_key` where newer rows
   have `event_name`.

2. **List the meter's events in its window.** Use the meter's `from` and
   `to`, not the period. They differ when a spend-threshold invoice already
   billed the start of the period, or when the meter was on the
   subscription for only part of it:

   ```bash
   CUSTOMER=<customer id>; METER=<meter_id>; FROM=<from>; TO=<to>
   curl -sS -G "$API/v1/usage-events" -H "Authorization: Bearer $KEY" \
     --data-urlencode "customer_id=$CUSTOMER" --data-urlencode "meter_id=$METER" \
     --data-urlencode "from=$FROM" --data-urlencode "to=$TO" \
     | jq -c '.data[] | {id, quantity, dimensions, timestamp, idempotency_key}'
   ```

   For a meter with several prices, `quantity` in the row adds up only the
   prices whose usage was below zero. Look at the events whose labels those
   prices take, such as `"model": "claude-sonnet-4.5"`.

3. **Find the corrections** (negative `quantity`) **and the usage they
   correct.** That usage may be in an earlier period.

## Fix

Each fix ends with an event that makes the period's stored usage match
what its invoices billed. Usage that arrives late for the period is billed
by [the late-usage steps](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice),
which bill the stored usage less what the invoices billed. Without that
event, they would bill it wrong by the amount of this fix.

- **The corrected usage was billed on an earlier invoice:**
  1. Create a credit note on that invoice (`POST /v1/credit-notes` with
     `invoice_id`, a `reason` and `lines`), then issue it
     (`POST /v1/credit-notes/{id}/issue`).
     - Credit the row's `quantity` (the part below zero, per price),
       priced as the earlier invoice priced it. The rest of the correction
       already cancelled out this period's usage.
     - For example: usage 100, a correction of -150, and the row shows
       -50. Credit 50 units.
     - With no `refund_amount_cents`, no money moves. On a paid invoice
       the credit goes to the customer's balance. On an unpaid one it
       lowers what is due ([Credit notes](../../concepts/invoices.md#credit-notes)).
       Before you set a refund, check the payment in Stripe: a refund made
       there already is recorded instead
       ([A refund made in Stripe](refund-made-in-stripe.md)).
  2. For each price you credited, [send an event](#send-the-matching-event)
     of the quantity you credited. The period's stored usage for that
     price is then what its invoice billed.
- **The correction was sent by mistake:** nothing was charged for it, but
  it cancelled out this period's real usage for that price.
  1. [Send an event](#send-the-matching-event) that reverses it: the
     correction's quantity, without the minus.
  2. Bill the usage the correction cancelled with
     [the late-usage steps](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice).
  3. Fix the sender so it does not repeat.

### Send the matching event

The event copies a correction's customer, meter, labels and time, so it
falls in the same period and the same price. For a credit, copy the
price's newest correction: the first one the Check's event list shows for
it. Its idempotency key comes from that correction's `id`, so a second
send stores nothing new. The backfill call takes an event for a period
already invoiced.

```bash
EXTERNAL_ID=<the customer's external_id>; EVENT_NAME=<the meter's event_name>
ID=<the correction's id>; TIMESTAMP=<its timestamp>; DIMENSIONS='<its dimensions>'
QUANTITY=<the quantity, without a minus>
jq -n --arg c "$EXTERNAL_ID" --arg e "$EVENT_NAME" --arg t "$TIMESTAMP" --arg k "below-zero-$ID" \
  --argjson q "$QUANTITY" --argjson d "$DIMENSIONS" \
  '{external_customer_id: $c, event_name: $e, quantity: $q, timestamp: $t, dimensions: $d, idempotency_key: $k}' \
  | curl -sS -X POST "$API/v1/usage-events/backfill" -H "Authorization: Bearer $KEY" \
    -H "Content-Type: application/json" -d @- | jq '{id, quantity, timestamp}'
```

**You see:** the stored event (`hc_evt_…`), dated at the correction's
time.

## Verify

The credit note is issued on the earlier invoice, and the customer's
balance or refund shows it. For a correction sent by mistake, the
late-usage one-off invoice is `finalized` or `paid`. Either way, the
event from [Send the matching event](#send-the-matching-event) is in the
Check's event list.

Related: [Invoice timing and late usage](invoice-timing-late-usage.md).
