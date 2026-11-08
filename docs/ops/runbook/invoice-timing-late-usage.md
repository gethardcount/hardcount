# Invoice timing and late usage

Hardcount builds a period's invoice only after a short wait, the invoice
grace period (`INVOICE_GRACE_PERIOD`, 1 hour by default), so usage that
arrives a little late still counts. Once built, an invoice never changes.
Usage for that period that arrives later is stored, and Hardcount never
bills it on its own. This page is for an invoice that is not built yet, or
usage missing from a built one. Its commands use `API` and `KEY` from
[Before you start](index.md#before-you-start).

## The invoice is not built yet

**Why:** the billing run is Hardcount's scheduled job that builds the
invoices that are due. Its first run after the grace period ends builds the
invoice ([how long that takes](../../api/timing-and-limits.md#when-invoices-are-made)).

- The same wait applies to `POST /v1/billing/run`, a billing run you start
  by hand, and to the final invoice of a cancel scheduled for a date.
- These build the invoice at once, with no grace: an immediate cancel,
  `POST /v1/subscriptions/{id}/retry-billing` on a subscription whose
  billing is failing, and moving a test clock (a pretend clock for test
  customers) forward.
- A change you make now to a subscription whose period has ended, such as
  a plan change, builds that period's invoice at once too. Usage still on
  its way then misses it.
- The invoice is still dated at the period end. Its webhook, tax record,
  use of prepaid credit and card charge come when it is built, and payment
  retries count from then.

**Check:**

```bash
SUB=<subscription id>
curl -sS "$API/v1/subscriptions/$SUB" -H "Authorization: Bearer $KEY" \
  | jq '{status, current_billing_period_end, next_billing_at, cancel_at, billing_failure}'
# The setting in force is logged at boot, in nanoseconds (3600000000000 = 1 hour):
#   {"msg":"invoice grace period","invoice_grace_period":3600000000000,...}
```

The subscription is due once `next_billing_at`, or a `cancel_at` inside
its current period, is at least `INVOICE_GRACE_PERIOD` in the past.
`billing_failure` is set when an attempt to bill it failed.

**Fix:**

- **Not due yet:** wait for the grace period and the next billing run.
- **Due, with a `billing_failure`:** go to [Billing failing](billing-failing.md).
- **Due, with no `billing_failure`:** go to
  [A subscription is due but not billed](background-job-stuck.md#a-subscription-is-due-but-not-billed).
- **A test needs a period closed right after it ends:** run a test server
  with `INVOICE_GRACE_PERIOD=0`. The setting applies to every
  subscription. `retry-billing` refuses a subscription whose billing is not
  failing, so it cannot close a healthy one early.

**Verify:** the check above shows `next_billing_at` in the next period, and
the invoice exists, dated at the period end.

## Usage is missing from a built invoice

**Why:** a built invoice never changes. An event stamped inside its period
that arrives later is stored, and only a one-off invoice, one you make by
hand, can bill it.

- Hardcount flags such an event. The API's answer to the request that sent
  it carries `billing_status: "after_close"`, and the answer to a batch or
  a LiteLLM delivery has an `after_close` count.
  `hardcount_usage_unbilled_total{reason="after_close"}` counts it (the
  counter only grows), and a WARN line in Hardcount's log names its
  customer, meter and time. There is no list of such events yet.
- The flag says only that the period was invoiced. It does not check that
  the plan priced the event's meter.
- Usage on a canceled subscription is not flagged `after_close`, even for
  a period that was invoiced: it counts as `outside_subscription`. The
  steps below find it too.
- A spend-threshold invoice bills a period's usage early, and the period's
  own invoice then bills only the usage stamped after it. Usage stamped
  before the spend-threshold invoice but arriving after it is on neither.
  While that invoice is not paid (open or written off), such usage is not
  flagged, so the counter misses it
  ([Spend thresholds](../../concepts/invoices.md#spend-thresholds-an-invoice-made-early)).
  The steps below find it too.

**Check:** `METRICS_TOKEN` is the token your server asks for on `/metrics`
([Settings](../../settings.md)). On Docker Compose, Hardcount's log is
`docker compose logs hardcount-api`.

```bash
# How many events arrived after their period's invoice, one line per origin (api or backfill):
curl -sS "$API/metrics" -H "Authorization: Bearer $METRICS_TOKEN" | grep '^hardcount_usage_unbilled_total{' | grep 'reason="after_close"'
# Which ones: the WARN line per event.
#   {"level":"WARN","msg":"usage event arrived after its period was invoiced — stored, billed on no invoice","customer_id":...,"meter_id":...,"timestamp":...}
```

**Fix:** bill the difference once, on a one-off invoice. These steps are
for a customer with one subscription. With more, step 3 compares only the
rows of `PLAN`, this subscription's plan.

1. **List the customer's invoices,** and keep the ones that are not voided:
   - the period's own invoice: the one built when the period ended. For a
     plan whose base fee is billed in advance (at the start of each
     period), it carries the next period's dates;
   - any spend-threshold invoice of the period (`billing_reason`
     `threshold`);
   - any one-off invoice (`billing_reason` `manual`) that step 4 made for
     this period: its memo starts `Late usage for` and the period's invoice
     number. Keep also one you know billed this usage. Never keep one made
     on another page, such as for
     [usage with no price](usage-with-no-price.md): that usage is not part
     of the step 3 total.

   Note also any voided spend-threshold invoice of the period: step 3
   needs it. If step 4 made a draft before, finalize that draft (step 5)
   rather than make another.

   ```bash
   CUSTOMER=<customer id>
   # With more than 100 invoices, add &offset=100, 200, ...
   curl -sS "$API/v1/invoices?customer_id=$CUSTOMER&limit=100" -H "Authorization: Bearer $KEY" \
     | jq -c '.data[] | {id, invoice_number, status, billing_reason, billing_period_start, billing_period_end, currency, memo, subtotal_cents, total_amount_cents, created_at, voided_at}'
   ```

   If the period's own invoice is not there, read the subscription with the
   check in [The invoice is not built yet](#the-invoice-is-not-built-yet):
   - **Its current period still holds the usage:** the invoice is not
     built yet. Stop, and come back once it is. It bills this usage,
     except usage stamped before a spend-threshold invoice of the period
     that is not voided.
   - **Its current period starts after the usage:** the period ended with
     nothing to bill, so it has no invoice. Go on. Step 3 takes the
     period's start and end, and step 4 takes the subscription ID as
     `INVOICE`, the period's start as `NUMBER` and the plan's currency.

2. **Save the usage lines of the period's own and spend-threshold
   invoices:**

   ```bash
   for INV in <the invoice ids>; do
     curl -sS "$API/v1/invoices/$INV" -H "Authorization: Bearer $KEY" \
       | jq -c '.line_items[] | select(.line_type == "usage")
           | {price_id, quantity, amount_cents, total_amount_cents, billing_period_start, billing_period_end}'
   done | tee lines.ndjson
   ```

3. **Compare them with the usage stored for the period.** `FROM` is the
   earliest `billing_period_start` of the step 2 lines, and `TO` the
   latest `billing_period_end` (`from` inclusive, `to` exclusive).
   - If the invoices have no usage line: use the dates of the period's own
     invoice. For a plan billed in advance, use those of the invoice built
     one period earlier instead: they are this period's.
   - If a spend-threshold invoice of the period was voided after the
     period's own invoice was built (its `voided_at` is later than that
     invoice's `created_at`), its usage is billed nowhere. Set `FROM` to
     the voided invoice's `billing_period_start`, the period start: its
     usage is then part of the difference below. If you voided it to
     forgive that usage, also subtract its usage lines from the total:
     their `amount_cents`, or their `total_amount_cents` when `INCLUSIVE`
     (below) is `true`. Read them with `GET /v1/invoices/<its id>`, not
     with the step 2 loop, which would replace `lines.ndjson`.

   The usage call prices the window at the plan's prices and the
   customer's overrides as they are now, not as they were then. So first
   list what changed since `FROM`:

   ```bash
   FROM=<earliest billing_period_start>; TO=<latest billing_period_end>
   PLAN=<the subscription's plan id>
   # With more than 100 changes, add &offset=100, 200, ...
   curl -sS -G "$API/v1/audit-log" -H "Authorization: Bearer $KEY" --data-urlencode "date_from=$FROM" \
     --data-urlencode "resource_type=plan_price" --data-urlencode "limit=100" \
     | jq -c --arg p "$PLAN" '.data[] | select(.metadata.plan_id == $p) | {created_at, action, meter_id: .metadata.meter_id}'
   curl -sS -G "$API/v1/audit-log" -H "Authorization: Bearer $KEY" --data-urlencode "date_from=$FROM" \
     --data-urlencode "resource_type=price_override" --data-urlencode "limit=100" \
     | jq -c --arg c "$CUSTOMER" '.data[] | select(.metadata.customer_id == $c) | {created_at, action, price_id: .metadata.price_id}'
   ```

   **You see:** nothing, when no price of the plan and no override of the
   customer changed. Otherwise, leave out of the total below every row of
   each meter a price line names, and the row of each price an override
   line names. These steps cannot price that usage at the period's
   prices. Add it to the total only if you can work out its amount at
   those prices; otherwise leave it unbilled.

   Then compare. `INCLUSIVE` is `true` when your prices include tax
   (tax-inclusive pricing, in **Settings → Tax**). An invoice line then
   holds the price less its tax in `amount_cents`, and the whole price in
   `total_amount_cents`:

   ```bash
   INCLUSIVE=$(curl -sS "$API/v1/settings" -H "Authorization: Bearer $KEY" | jq .tax_inclusive)
   curl -sS -G "$API/v1/customers/$CUSTOMER/usage" -H "Authorization: Bearer $KEY" \
     --data-urlencode "from=$FROM" --data-urlencode "to=$TO" > window.json
   jq -r --slurpfile lines lines.ndjson --arg plan "$PLAN" --argjson inc "$INCLUSIVE" '
     (.warnings[] | select(test("below zero")) | "warning: " + .),
     (.meters[] | .event_name as $meter | .prices[]
       | select(.plan_id == $plan and (.no_price | not)) | . as $row
       | [$lines[] | select(.price_id == $row.price_id)] as $billed
       | [$meter, .price_id,
          (.quantity | tonumber) - ($billed | map(.quantity | tonumber) | add // 0),
          .amount_cents - ($billed | map(if $inc then .total_amount_cents else .amount_cents end) | add // 0)]
       | @tsv)' window.json
   ```

   **You see:** a line for each price of the plan with usage: its meter,
   its `price_id`, the quantity the step 2 lines did not bill, and the
   cents it adds.
   - **Quantity 0:** nothing of that price is late. Leave its cents out:
     each invoice line was rounded on its own.
   - **Quantity above 0:** usage that missed the invoices. Its cents are
     what to bill.
   - **Quantity below 0, or a `warning` that a meter is below zero:** a
     correction arrived after the invoice, so the invoice billed too much.
     It is not part of the total: see [Usage below zero](usage-below-zero.md).
   - Usage that no price took is left out:
     [Usage with no price](usage-with-no-price.md).

   Add up the cents to bill, and subtract the one-off invoices step 1
   kept: their `subtotal_cents`, or their `total_amount_cents` when
   `INCLUSIVE` is `true`. The rest is the total to bill. Hardcount works
   out its tax when you finalize.

   **A canceled subscription:** the usage call prices only active or
   trialing subscriptions, so it answers `meters: []`. Read each price's
   quantity instead: `total_units` from
   `GET /v1/usage-events/aggregate?customer_id=$CUSTOMER&meter_id=<meter id>&from=$FROM&to=$TO&dimensions=<key>=<value>,...`,
   with the labels of the price's `dimension_match`
   (`GET /v1/plans/$PLAN/prices` lists them). Where one price's labels
   include all of another's, take the other's quantity off. Compare it
   with the price's step 2 lines, and price what is higher as the line
   priced its quantity: for a price per unit, the extra quantity times
   the line's `unit_amount_decimal` cents.

4. **Create one one-off invoice for the total.** `N` goes into the memo
   (`MEMO`) and the idempotency key (`IDEMPOTENCY_KEY`), so each one-off
   invoice for this period gets its own. If the command gets no answer,
   send it again within 24 hours: the `Idempotency-Key` returns the same
   draft. After that, start again from step 1.

   ```bash
   INVOICE=<the period's invoice id>; NUMBER=<its invoice_number>; CURRENCY=<its currency>
   N=<one-off invoices that step 1 kept, plus 1>; CENTS=<the total>
   MEMO="Late usage for $NUMBER ($N)"
   IDEMPOTENCY_KEY="late-usage-$INVOICE-$N"
   jq -n --arg c "$CUSTOMER" --arg cur "$CURRENCY" --arg m "$MEMO" \
     --argjson cents "$CENTS" '{customer_id: $c, currency: $cur, memo: $m, line_items: [{description: $m,
       line_type: "usage", quantity: 1, unit_amount_cents: $cents}]}' > late.json
   curl -sS -X POST "$API/v1/invoices" -H "Authorization: Bearer $KEY" \
     -H "Content-Type: application/json" -H "Idempotency-Key: $IDEMPOTENCY_KEY" \
     -d @late.json | jq '{id, status, currency, total_amount_cents}'
   ```

   **You see:** a `draft` (`hc_inv_…`) in `CURRENCY`, whose
   `total_amount_cents` is `CENTS`.

5. **Check the draft's amount, then finalize it.** Finalizing charges the
   customer at once: prepaid credit first, then the saved card.

   ```bash
   curl -sS -X POST "$API/v1/invoices/<draft id>/finalize" -H "Authorization: Bearer $KEY" \
     | jq '{status, payment_status, amount_due_cents}'
   ```

   **You see:** `"status": "finalized"`, or `"paid"` when credit or the
   card paid it at once, and the amount due.

**To stop it happening again:** if a pipeline often delivers later than
the grace, raise `INVOICE_GRACE_PERIOD` above its worst delay (a duration
such as `3h`, at most `72h`) and restart. Every period-end invoice then
comes that much later.

**Verify:** step 1's list shows the one-off invoice, `finalized` or `paid`,
with its memo.

## Other things the grace delays

- **Dunning's final step.** Dunning is Hardcount's retrying of a failed
  payment. When the retries run out while a period's grace is running, the
  final pause or cancel waits until that grace ends. The invoice page shows
  that time as "Final step", and the Dunning page as "Final step in …".
  No charge is retried then: the pause or cancel runs.
- **A cancel or plan change scheduled for a date.** It takes effect when
  Hardcount builds that period's invoice, up to one grace period after the
  date. Until then the subscription is `active` on its old plan.
  `subscription.canceled` is sent then, with `canceled_at` at the scheduled
  date. A scheduled end of a collection pause (a pause in charging the
  customer) that falls after a period end waits for that period's invoice
  too.
- **The subscription's period.** During the grace the subscription still
  shows the ended period. Spend thresholds and the current credit balance
  do not count the new period's usage until the invoice is built.

Related: [ADR-120](../../adr/120-invoice-grace-period-for-late-usage.md).
