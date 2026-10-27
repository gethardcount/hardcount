# Usage below zero

Velox accepts negative usage events as corrections. When the corrections in
a billing period are larger than the usage they offset, that meter's usage
for the period is below zero. Back to the [runbook index](index.md).

## Symptom

- The subscription's activity timeline shows **Usage below zero billed as
  zero**, with each meter and its amount, for example `api_calls -50`. The
  audit log has the same row, action `subscription.net_negative_usage`.
- Or the server logs a WARN `cycle: usage below zero after corrections —
  billed as 0` with `meter`, `quantity` and `subscription_id`. The prefix is
  `cycle`, `cancel` or `threshold`.
- The invoice preview and the customer usage view warn that the meter's
  usage is below zero and counted as 0.

## Why

A negative quantity has no price, so Velox counts that meter as 0 for the
period and builds the rest of the invoice: base fees and every other meter.
It does not carry the negative amount into a later period.

- One row is written per invoice built this way: the period close, the
  final invoice of an immediate cancel, or a spend-threshold invoice. It is
  written in the same transaction as the invoice, so a retried close writes
  it once. A close with nothing else to bill makes no invoice and still
  writes the row.
- The rule applies to each meter, or to each price of a meter such as one
  model of a tokens meter, on its own. Another meter's usage is not
  reduced.
- A common cause is a correction for usage that an earlier invoice already
  billed. The correction then lands in a later period, where there is less
  usage to offset.

## Check

```bash
# The events of that meter in its window (the meter's from and to in the row):
curl -sS "$API/v1/usage-events?customer_id=$CUSTOMER&meter_id=$METER&from=$FROM&to=$TO" \
  -H "Authorization: Bearer $KEY"
```

- The row's metadata has `meters` (`meter_id`, `meter_key`, `quantity`,
  `from`, `to`; `meter_key` holds the meter's `event_name`), `billing_reason`, `period_start`, `period_end`, and
  `invoice_id` and `invoice_number` when an invoice was made.
- Use the meter's `from` and `to`, not the period. They differ when a
  spend-threshold invoice already billed the start of the period, or when
  the meter was on the subscription for only part of it.
- For a meter with several prices on the plan, `quantity` adds up only the
  prices whose usage was below zero. Group the meter's events by the labels
  those prices match.
- Find the corrections (negative `quantity`) and the usage they correct.
  Their usage events may be in an earlier period.

## Fix

- **The corrected usage was billed on an earlier invoice:** create a credit
  note on that invoice for the corrected amount
  (`POST /v1/credit-notes` with `invoice_id` and `lines`), then issue it
  (`POST /v1/credit-notes/{id}/issue`).
- **The correction was sent by mistake:** nothing was charged for it. Fix
  the sender so it does not repeat.

## Verify

The credit note is issued on the earlier invoice, and the customer's
balance or refund shows it.

Related: [Invoice timing and late usage](invoice-timing-late-usage.md).
