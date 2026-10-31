# Test a billing change with a test clock

This guide tries a price change on a test customer and shows the invoices
it makes, months ahead, in a few minutes. Use it before you change a price
or plan that real customers are on.

It runs in test mode, so nothing charges real money. The dates are fixed,
so you can check every number: a customer starts on January 1, you raise
the price on January 15, and you read the January and February invoices.

## Before you start

- Hardcount running, with `HARDCOUNT` and `HARDCOUNT_KEY` set to its address
  and a test secret key, as in
  [first invoice](../first-invoice.md#before-you-start).
- A `tokens` meter. Step 1 of the [first invoice
  page](../first-invoice.md#steps) creates one with a recipe.
- `curl` and `jq`.
- Your account's billing timezone is UTC, the default. With another
  timezone, the periods below start at midnight in that timezone instead.

What a test clock is and what it runs: [Test clocks](../concepts/test-clocks.md).

## Steps

1. **Pick a name for this run, and find the `tokens` meter.** The name
   goes into the plan's code, the customer's external ID and the
   subscription's code, which must each be new. To run the page again,
   pick another name, such as `price-test-2`.

   ```bash
   REF=price-test-1
   METER_ID=$(curl -s "$HARDCOUNT/v1/meters?limit=100" -H "Authorization: Bearer $HARDCOUNT_KEY" \
     | jq -r '.data[] | select(.event_name == "tokens") | .id')
   echo "$METER_ID"
   ```

   **You see:** the meter ID, such as `hc_mtr_…`.

2. **Create a plan just for this test.** A test plan keeps the change away
   from your other test customers. It has no base fee and one price: Claude
   Sonnet 4.5 input tokens at `0.0003` cents, $3.00 per million.

   ```bash
   PLAN=$(jq -n --arg meter "$METER_ID" --arg ref "$REF" \
       '{code: $ref, name: "Price change test", currency: "USD",
         billing_interval: "monthly", base_amount_cents: 0,
         prices: [{meter_id: $meter,
                   dimension_match: {model: "claude-sonnet-4.5", token_type: "input"},
                   mode: "per_unit", unit_amount_cents: "0.0003"}]}' \
     | curl -s -X POST "$HARDCOUNT/v1/plans" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @-)
   PLAN_ID=$(echo "$PLAN" | jq -r .id)
   PRICE_ID=$(echo "$PLAN" | jq -r '.prices[0].id')
   echo "$PLAN_ID $PRICE_ID"
   ```

   **You see:** the two IDs, such as `hc_pln_… hc_prc_…`.

3. **Create a test clock on January 1, 2026.**

   ```bash
   CLOCK_ID=$(curl -s -X POST "$HARDCOUNT/v1/test-clocks" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" \
     -H "Content-Type: application/json" \
     -d '{"name": "price change test", "frozen_time": "2026-01-01T00:00:00Z"}' | jq -r .id)
   echo "$CLOCK_ID"
   ```

   **You see:** the clock ID, such as `hc_tclk_…`.

4. **Create a customer on the clock, and put it on the plan.** The
   subscription starts at the clock's time, so its first period is
   January 1 to February 1.

   ```bash
   CUSTOMER_ID=$(jq -n --arg clock "$CLOCK_ID" --arg ref "$REF" \
       '{external_id: $ref, display_name: "Price Test", test_clock_id: $clock}' \
     | curl -s -X POST "$HARDCOUNT/v1/customers" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .id)
   jq -n --arg cus "$CUSTOMER_ID" --arg plan "$PLAN_ID" --arg ref "$REF" \
       '{code: $ref, display_name: "Price Test", customer_id: $cus,
         items: [{plan_id: $plan, quantity: 1}]}' \
     | curl -s -X POST "$HARDCOUNT/v1/subscriptions" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .status
   ```

   **You see:** `active`.

5. **Save two shell functions** for the steps below. `send_input` sends
   input tokens at the clock's time. `advance` moves the clock and waits
   until it is done.

   ```bash
   send_input() {   # send_input <key> <quantity>
     jq -n --arg ref "$REF" --arg key "$1" --arg qty "$2" \
         '{event_name: "tokens", external_customer_id: $ref, quantity: $qty,
           idempotency_key: ($ref + "-" + $key),
           dimensions: {model: "claude-sonnet-4.5", token_type: "input"}}' \
       | curl -s -X POST "$HARDCOUNT/v1/usage-events" \
         -H "Authorization: Bearer $HARDCOUNT_KEY" \
         -H "Content-Type: application/json" \
         -d @- | jq -c '{quantity, timestamp}'
   }
   advance() {   # advance <time>
     jq -n --arg t "$1" '{frozen_time: $t}' \
       | curl -s -X POST "$HARDCOUNT/v1/test-clocks/$CLOCK_ID/advance" \
         -H "Authorization: Bearer $HARDCOUNT_KEY" \
         -H "Content-Type: application/json" -d @- > /dev/null
     while :; do
       status=$(curl -s "$HARDCOUNT/v1/test-clocks/$CLOCK_ID" \
         -H "Authorization: Bearer $HARDCOUNT_KEY" | jq -r .status)
       [ "$status" != advancing ] && break
       sleep 1
     done
     echo "$status"
   }
   ```

6. **Send January's first usage, on January 1.**

   ```bash
   send_input jan-a 500000
   ```

   **You see:** `{"quantity":"500000","timestamp":"2026-01-01T00:00:00Z"}`

7. **Move the clock to January 15.**

   ```bash
   advance 2026-01-15T00:00:00Z
   ```

   **You see:** `ready`. If you see `internal_failure`, read the clock's
   `last_failure_reason`.

8. **Raise the price to $4.00 per million** (`0.0004` cents).

   ```bash
   curl -s -X PATCH "$HARDCOUNT/v1/plans/$PLAN_ID/prices/$PRICE_ID" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" \
     -H "Content-Type: application/json" \
     -d '{"mode": "per_unit", "unit_amount_cents": "0.0004"}' \
     | jq -c '{unit_amount_cents, effective}'
   ```

   **You see:** `{"unit_amount_cents":"0.0004","effective":"next_period"}`.
   The plan has a subscriber, so the change applies from each subscriber's
   next billing period. January keeps the price it started with.

9. **Send January's second usage, on January 15.**

   ```bash
   send_input jan-b 500000
   ```

   **You see:** `{"quantity":"500000","timestamp":"2026-01-15T00:00:00Z"}`

10. **Move the clock past the end of January, and read the invoice.**

    ```bash
    advance 2026-02-02T00:00:00Z
    curl -s "$HARDCOUNT/v1/invoices?customer_id=$CUSTOMER_ID" \
      -H "Authorization: Bearer $HARDCOUNT_KEY" \
      | jq -c '.data[] | {start: .billing_period_start, total_cents: .total_amount_cents}'
    ```

    **You see:** `ready`, then the January invoice:
    `{"start":"2026-01-01T00:00:00Z","total_cents":300}`.
    Both halves of January bill at the old price:
    (500,000 + 500,000) × 0.0003 cents = 300 cents, $3.00.

11. **Send February's usage, and move the clock past the end of February.**

    ```bash
    send_input feb 1000000
    advance 2026-03-02T00:00:00Z
    curl -s "$HARDCOUNT/v1/invoices?customer_id=$CUSTOMER_ID" \
      -H "Authorization: Bearer $HARDCOUNT_KEY" \
      | jq -c '.data[] | {start: .billing_period_start, total_cents: .total_amount_cents}'
    ```

    **You see:** the usage event, `ready`, then both invoices, newest first:

    ```text
    {"start":"2026-02-01T00:00:00Z","total_cents":400}
    {"start":"2026-01-01T00:00:00Z","total_cents":300}
    ```

    February bills at the new price: 1,000,000 × 0.0004 cents = 400
    cents, $4.00.

Open the customer in the dashboard to see both invoices and their lines.
Home leaves out customers on a test clock, so its totals do not change.

## Try other changes the same way

- **A plan change:** `PATCH /v1/subscriptions/{id}/items/{itemID}` with
  `new_plan_id`, between steps 7 and 10. With `"immediate": true` it
  applies on January 15; without it, on February 1. How the base fee is
  prorated: [Subscriptions](../concepts/subscriptions.md#plan-changes).
- **A dunning policy:** a customer with a card that declines, and moves to
  each retry date. A policy that pauses or cancels needs a move in two
  steps: [Test clocks](../concepts/test-clocks.md#testing-a-dunning-policy-that-pauses-or-cancels).
- **Late usage:** an event with a January `timestamp`, sent after step 10,
  is answered with `billing_status: "after_close"` and is on no invoice
  ([Late usage](../concepts/usage-events.md#late-usage)).

## Clean up

`DELETE /v1/test-clocks/{id}` deletes the clock, the customer and
everything it owns. The plan stays: archive it under **Pricing** if you no
longer need it.

```bash
curl -s -X DELETE "$HARDCOUNT/v1/test-clocks/$CLOCK_ID" -H "Authorization: Bearer $HARDCOUNT_KEY"
```

**You see:** `{"status":"deleted"}`

More: the rules for when a price change applies:
[Change a price](../ai-pricing.md#change-a-price).
