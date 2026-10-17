# Usage with no price

Velox prices each usage event with its meter's pricing rules. Usage that
matches no rule, on a meter with no default price, is left off the invoice.
This page is for finding and billing it. Back to the
[runbook index](index.md).

## Symptom

`velox_usage_unpriced_quantity_total{meter_id, mode}` rises (`mode` is
`live` or `test`; test-clock simulations are not counted). Or the server
logs a WARN with `meter`, `quantity` and `subscription_id`, in one of two
shapes. The prefix is `cycle`, `cancel` or `threshold`:

- `cycle: usage matched no rating rule and meter has no default binding —
  not billed`: the meter has pricing rules, and none matched.
- `cycle: usage on a meter with no rating rule — not billed (unpriced
  meter on plan; ADR-096)`: the meter has no rules and no default price.

Both contain "no rating rule" and "not billed".

## Why

The usage matched no pricing rule on its meter, and the meter has no
default price. A common cause is a new model name on an AI meter that has
rules for the older models only.

- Velox builds and sends the invoice without that usage, as Lago,
  Metronome and Orb do. It does not bill it later on its own.
- The counter adds the quantity once per invoice built without it: period
  close, cancel final invoice or spend-threshold invoice, once that
  invoice's transaction commits.
- The counter resets when the process restarts.

## Check

```bash
# Which meter is it, and which customers have it this period?
curl -sS "$API/v1/meters/$METER" -H "Authorization: Bearer $KEY" | jq '{key, rating_rule_version_id}'
curl -sS "$API/v1/customers/$CUSTOMER/usage" -H "Authorization: Bearer $KEY" \
  | jq '.meters[] | {meter_key, unmatched: [.rules[] | select(.unmatched == true) | .quantity]}'
# Which dimension values are unpriced (e.g. the model name):
curl -sS "$API/v1/usage-events?customer_id=$CUSTOMER&meter_id=$METER" -H "Authorization: Bearer $KEY"
```

- Without `from` and `to`, the customer usage call covers the current
  cycle. A WARN at period close is about the period that just ended, so add
  `?from=<billing_period_start>&to=<billing_period_end>` of that invoice to
  see it.
- For a canceled subscription the call returns no meters; use the
  usage-events call with the same `from` and `to`. See
  [Invoice timing and late usage](invoice-timing-late-usage.md) for details.
- The customer page shows the same "unmatched usage — not billed" row. The
  `subscription_id` and `quantity` in the WARN tell you which invoice left
  it out.

## Fix

1. **Add a price so later invoices bill it:** a pricing rule for the new
   value (`POST /v1/meters/{id}/pricing-rules`), or a default price for the
   meter (`PATCH /v1/meters/{id}` with `rating_rule_version_id`). Usage in
   a period whose invoice is not built yet is then billed on it.
2. **Bill usage from periods already invoiced** with a one-off invoice:
   `POST /v1/invoices`, then `POST /v1/invoices/{id}/line-items`, then
   `POST /v1/invoices/{id}/finalize`. Take the quantity from the WARN, not
   from the usage view after step 1. Rules are matched when usage is read,
   so once the price exists the view shows past usage as priced, although
   its invoice did not bill it.

## Verify

The next invoice for the customer carries a line for the usage, and the
counter stops rising for that meter.

Related: [ADR-096](../../adr/096-unpriced-meter-plan-attach-guard.md).
