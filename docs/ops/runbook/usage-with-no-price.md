# Usage with no price

Hardcount prices each usage event with the prices of the customer's plan. Usage
of a meter that no price of the plan takes is left off the invoice.
Back to the [runbook index](index.md).

## Symptom

| When | What you see |
|---|---|
| The event arrives | `billing_status: "no_price"` in the API's answer, and `hardcount_usage_unbilled_total{reason="no_price"}` rises |
| The invoice is built | `hardcount_usage_unpriced_quantity_total{meter_id, mode}` rises, and the server logs a WARN `cycle: usage matched no price of its plan — not billed` |

- The answer is the API's response to the request that sent the event. A
  batch, a LiteLLM delivery and a release of
  [held usage](../../integrations/litellm.md#usage-for-a-customer-hardcount-does-not-know-yet)
  carry a `no_price` count instead.
- Usage of a meter the plan has no price for at all is `not_on_plan`. The
  invoice-time WARN and counter leave it out, and so does the customer
  usage call. The Fix below applies to it too: find its events with the
  usage-events call in [Check](#check).
- `mode` is `live` or `test`. Test-clock simulations are not counted.
- The WARN carries `meter`, `quantity` and `subscription_id`. Its prefix is
  `cycle`, `cancel` or `threshold`, and the `cycle` and `cancel` lines also
  carry `plan_id`.

Alert on `hardcount_usage_unpriced_quantity_total`: it counts usage that an
invoice left off. The first row is what Hardcount found when the event
arrived, an early sign and not a promise. An event with no
`billing_status` can still end up with no price.

## Why

The plan has prices for the meter, but none takes these events. A price
takes an event whose labels include every label of the price, so a common
cause is a new model name on an AI meter whose plan has prices for the
older models only.

- Hardcount builds and sends the invoice without that usage. It does not
  bill it later on its own.
- The counter adds the quantity once for each invoice built without it: the
  invoice at the period's end, a cancel's final invoice, or a
  [spend-threshold invoice](../../concepts/invoices.md#spend-thresholds-an-invoice-made-early).
- The counter resets when the process restarts.

## Check

```bash
PLAN=<plan id>; CUSTOMER=<customer id>; METER=<meter id>
# The plan's prices, 100 a page (with more, add &offset=100, 200, ...),
# and the customer's usage no price took this period:
curl -sS "$API/v1/plans/$PLAN/prices?limit=100" -H "Authorization: Bearer $KEY" | jq '.data[] | {meter_id, dimension_match}'
curl -sS "$API/v1/customers/$CUSTOMER/usage" -H "Authorization: Bearer $KEY" \
  | jq '.meters[] | {event_name, no_price: [.prices[] | select(.no_price == true) | {plan_id, quantity}]}'
# Which label values have no price (e.g. the model name):
curl -sS "$API/v1/usage-events?customer_id=$CUSTOMER&meter_id=$METER" -H "Authorization: Bearer $KEY"
```

- Without `from` and `to`, the customer usage call covers the current
  period. A WARN at a period's end is about the period that just ended, so
  add `?from=<start>&to=<end>` of that period to see it. For a plan billed
  in advance, the invoice carries the next period's dates: take the window
  as in step 3 of
  [Usage is missing from a built invoice](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice).
- For a canceled subscription the call returns no meters; use the
  usage-events call with the same `from` and `to`. See
  [Invoice timing and late usage](invoice-timing-late-usage.md) for details.
- The customer page shows the same "No price matches this usage — not
  billed" row. The `subscription_id` and `quantity` in the WARN tell you
  which invoice left it out.

## Fix

1. **Add a price so later periods bill it:** `POST /v1/plans/{id}/prices`
   with the meter and the labels of the usage, or with no labels to take
   every event of the meter that no other price takes. On a plan no one
   has subscribed to, the price applies at once. On a used plan, it applies
   from each subscriber's next billing period. The period already open
   keeps the prices it started with, so its usage with no price is still
   not billed.
2. **Bill the usage that was left out.** Each WARN line gives the
   quantity one invoice left out: `quantity`, with its `subscription_id`
   and `meter`. For the period that is open now, wait until its invoice is
   built and use its line.
   - Bill every line of each invoice that is not voided, with one
     exception. A `threshold` line whose spend-threshold invoice was voided
     before the period's own invoice was built is counted again on that
     invoice's line (`cycle` or `cancel`): skip it.
   - `CENTS` is that quantity at the price you added in step 1, or at the
     price you meant to charge.
   - If the log is gone, read the quantity from the `no_price` row of the
     customer usage call over the period, with `FROM` and `TO` as in
     step 3 of
     [Usage is missing from a built invoice](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice).
     It covers every invoice of the period together: subtract what you
     already billed from that period's WARN lines. The call prices at
     the prices of the subscription's current period: once the price from
     step 1 applies there, this usage shows on that price's row instead.

   Bill it with a one-off invoice, as in steps 4 and 5 of
   [Usage is missing from a built invoice](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice).
   `INVOICE` and `NUMBER` are the invoice that left the usage out. Use this
   memo and idempotency key there, with `N` counting only such invoices
   for that invoice, so the late-usage steps never count this one as
   theirs:

   ```bash
   MEMO="Usage with no price on $NUMBER ($N)"; IDEMPOTENCY_KEY="no-price-$INVOICE-$N"
   ```

## Verify

From the next billing period, the customer's invoice carries a line for the
usage, and the counter stops rising for that meter.

Related: [ADR-130](../../adr/130-usage-prices-belong-to-the-plan.md).
