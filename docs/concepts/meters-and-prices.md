# Meters and prices

A meter adds up one kind of usage. A plan's prices turn that total into
money. This page explains both, how Hardcount picks the price for each
event, and how a base fee is billed.

## Meters

A meter is a counter for one kind of usage, such as tokens, API requests
or GPU seconds. It has:

- an `event_name`, the name usage events send to say "count me here", such
  as `tokens`;
- a `unit`, shown on invoices, such as `tokens`;
- one aggregation: how it combines a period's events into one quantity.

| Aggregation | The period's quantity is | Use it for |
|---|---|---|
| `sum` | The quantities added together. | Tokens, requests, GB transferred. |
| `count` | The number of events. Their quantities are ignored. | One event per action. |
| `max` | The highest quantity. | Peak seats or peak storage. |
| `last` | The quantity of the latest event, by `timestamp`. | A reading, such as seats at the end of the month. |

A meter only counts. It holds no price, so two plans can price one meter
differently.

You can change a meter's aggregation until it has its first usage event.
After that, a change is refused with `409 meter_has_usage`, because its
usage so far was counted the old way. Usage that Hardcount holds for an
unknown customer does not count until it is released.

## Prices

A price is one usage rate of one plan. It names a meter, the labels an
event must carry for the price to take it (`dimension_match`), and a rate.
It is always in the plan's currency.

A rate has one of three shapes, its `mode`:

| `mode` | Bills | Fields |
|---|---|---|
| `per_unit` | quantity × `unit_amount_cents` | `unit_amount_cents` |
| `graduated` | each tier's share of the quantity at the tier's rate | `tiers`: each `{up_to, unit_amount_cents}`; the last tier has `up_to: null` |
| `package` | whole packages × `package_amount_cents`; a part package counts as a whole one | `package_size`, `package_amount_cents` |

Amounts are in cents, the currency's smallest unit, and can have up to 12
decimal places. So `"0.0003"` cents per token is $3.00 per million tokens.
Each invoice line is rounded to whole cents once, at the end. A half cent
rounds to the even cent: 262.5 cents becomes 262, and 263.5 becomes 264.

## How Hardcount picks the price for an event

Each event is billed by at most one price of the customer's plan.

1. **A price takes an event whose labels include all of the price's
   labels.** A price with `{"model": "gpt-4o"}` takes every `gpt-4o`
   event, whatever its `token_type`. A price with no labels takes every
   event of its meter.
2. **When several prices can take an event, the one with the most labels
   wins.** So nothing is billed twice. A broad price and finer prices can
   live side by side: the finer ones win where they apply.
3. **Two prices that would tie are refused.** Two prices with the same
   number of labels that could both take one event, such as
   `{"model": "a"}` and `{"token_type": "input"}`, have no winner. The
   second one is refused with `409 price_conflict`.
4. **An event that no price takes is not billed.** Hardcount stores it,
   answers `billing_status: "no_price"`, and shows it as usage with no
   price. The usual cause is a new model name.

Labels are compared as text: the number `1` on an event does not match
`"1"` on a price.

More: [AI pricing reference](../ai-pricing.md#how-hardcount-picks-the-price-for-an-event).

## Prices belong to the plan

Each plan has its own prices, so two plans can charge different rates for
one meter. One customer cannot have two live subscriptions that price the
same meter, so no usage is billed twice. The second subscription is refused
with `409`. So is a new price that would add the meter to a plan such a
customer is on (`409 meter_overlap`).

A price change takes effect at a period boundary, so a period is never
billed at two rates:

- On a plan no subscription has used, the change applies at once
  (`effective: now`).
- On a used plan, it applies from each subscriber's next billing period
  (`effective: next_period`).
- A subscriber who joins the plan after the change pays the new rate from
  the moment they join.

More: [Change a price](../ai-pricing.md#change-a-price).

## The base fee, and when it is billed

A plan can also have a fixed base fee, `base_amount_cents`, such as $49.00
a month. A plan with a base fee must say when it is billed, in
`base_bill_timing`:

- `in_advance`: at the start of the period it covers;
- `in_arrears`: at the end, on the same invoice as the period's usage.

There is no default: a plan with a base fee and no `base_bill_timing` is
refused with `422`. A plan with no base fee bills in arrears. Usage is
always billed in arrears, at the end of the period, because its quantity is
known only then.

## Worked example: usage in, invoice lines out

An "AI API" plan in USD has a $20.00 base fee billed in arrears, and three
prices on a `tokens` meter (`sum`):

| Price | `dimension_match` | Rate |
|---|---|---|
| A | `{"model": "claude-sonnet-4.5", "token_type": "input"}` | `per_unit`, `0.0003` cents ($3.00 per million) |
| B | `{"model": "claude-sonnet-4.5", "token_type": "output"}` | `per_unit`, `0.0015` cents ($15.00 per million) |
| C | `{"model": "claude-sonnet-4.5"}` | `per_unit`, `0.0003` cents |

In June the customer's events add up to:

- 1,000,000 tokens with `token_type: input`;
- 175,000 tokens with `token_type: output`;
- 200,000 tokens with `token_type: cache_read`.

Prices A and C both fit the input events. A has two labels and C has one,
so A wins. The same goes for B and the output events. Only C fits the
cache-read events. The invoice lines are:

| Line | Sum | Amount |
|---|---|---|
| Base fee | | $20.00 |
| Price A | 1,000,000 × 0.0003 = 300 cents | $3.00 |
| Price B | 175,000 × 0.0015 = 262.5 cents, rounded to the even cent | $2.62 |
| Price C | 200,000 × 0.0003 = 60 cents | $0.60 |

The subtotal is $20.00 + $3.00 + $2.62 + $0.60 = $26.22, before any tax.

The other two shapes, on a `requests` meter:

- **Graduated**, tiers `{up_to: 1000000, unit_amount_cents: "0.01"}` and
  `{up_to: null, unit_amount_cents: "0.005"}`, for 1,500,000 requests:
  1,000,000 × 0.01 = 10,000 cents, plus 500,000 × 0.005 = 2,500 cents.
  The line is 12,500 cents, $125.00.
- **Package**, `package_size: 1000` and `package_amount_cents: "50"`, for
  2,500 requests: 2,500 ÷ 1,000 = 2.5, so 3 packages × 50 cents = 150
  cents, $1.50.

**Next:** how a subscription puts a customer on a plan:
[Subscriptions](subscriptions.md).
