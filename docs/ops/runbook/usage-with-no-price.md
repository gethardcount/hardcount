# Usage with no price

Velox prices each usage event with the prices of the customer's plan. Usage
of a meter that no price of the plan takes is left off the invoice.
Back to the [runbook index](index.md).

## Symptom

The earliest sign is at ingest: the answer that wrote the event carries
`billing_status: "no_price"` (a `no_price` count on a batch, a LiteLLM
delivery or a held-usage release), and
`velox_usage_unbilled_total{reason="no_price"}` rises. Usage of a meter the
plan has no price for at all is answered `not_on_plan` instead. These are
what Velox found when the event arrived. Ingest does not check every event,
and an event with no status is not promised to be billed: the invoice signs
below are the final answer.

At the invoice, `velox_usage_unpriced_quantity_total{meter_id, mode}` rises (`mode` is
`live` or `test`; test-clock simulations are not counted). Or the server
logs a WARN with `meter`, `quantity` and `subscription_id`:

```
cycle: usage matched no price of its plan — not billed
```

The prefix is `cycle`, `cancel` or `threshold`. The `cycle` and `cancel`
lines also carry `plan_id`.

## Why

The plan has prices for the meter, but none takes these events, or it has
no price for the meter at all. A price takes an event whose labels include
every label of the price, so a common cause is a new model name on an AI
meter whose plan has prices for the older models only.

- Velox builds and sends the invoice without that usage. It does not bill
  it later on its own.
- The counter adds the quantity once per invoice built without it: period
  close, cancel final invoice or spend-threshold invoice, once that
  invoice's transaction commits.
- The counter resets when the process restarts.

## Check

```bash
# The plan's prices, 100 a page (with more, add &offset=100, 200, ...),
# and the customer's usage no price took this period:
curl -sS "$API/v1/plans/$PLAN/prices?limit=100" -H "Authorization: Bearer $KEY" | jq '.data[] | {meter_id, dimension_match}'
curl -sS "$API/v1/customers/$CUSTOMER/usage" -H "Authorization: Bearer $KEY" \
  | jq '.meters[] | {event_name, no_price: [.prices[] | select(.no_price == true) | {plan_id, quantity}]}'
# Which label values have no price (e.g. the model name):
curl -sS "$API/v1/usage-events?customer_id=$CUSTOMER&meter_id=$METER" -H "Authorization: Bearer $KEY"
```

- Without `from` and `to`, the customer usage call covers the current
  cycle. A WARN at period close is about the period that just ended, so add
  `?from=<billing_period_start>&to=<billing_period_end>` of that invoice to
  see it.
- For a canceled subscription the call returns no meters; use the
  usage-events call with the same `from` and `to`. See
  [Invoice timing and late usage](invoice-timing-late-usage.md) for details.
- The customer page shows the same "No price matches this usage — not
  billed" row. The `subscription_id` and `quantity` in the WARN tell you
  which invoice left it out.

## Fix

1. **Add a price so later periods bill it:** `POST /v1/plans/{id}/prices`
   with the meter and the labels of the usage, or with no labels to take
   every event of the meter that no other price takes. On a plan no one has
   subscribed to it applies at once. On a used plan it applies from each
   subscriber's next billing period: the period already open keeps the
   prices it started with, so its usage with no price is still not billed.
2. **Bill the usage that was left out** with a one-off invoice:
   `POST /v1/invoices`, then `POST /v1/invoices/{id}/line-items`, then
   `POST /v1/invoices/{id}/finalize`. Take the quantity from the WARN for a
   period already invoiced, and from the `no_price` row for the open one
   once its invoice is built.

## Verify

From the next billing period, the customer's invoice carries a line for the
usage, and the counter stops rising for that meter.

Related: [ADR-130](../../adr/130-usage-prices-belong-to-the-plan.md).
