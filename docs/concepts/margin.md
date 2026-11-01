# Margin

Margin is what a customer's usage earned you, minus what it cost you, as a
share of what it earned. Hardcount works it out per customer and per model,
from your prices and the costs you pay your AI provider. Only you see it,
never the customer.

## Costs: what each event cost you

A provider cost is what an AI provider charges you per token, for one
provider, model and kind of token. Save one with `PUT /v1/provider-costs`:

```json
{"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "input", "unit_cost_cents": "0.00012"}
```

`unit_cost_cents` is in US cents per token: `0.00012` cents is $1.20 per
million tokens. Provider costs are always in USD.

Hardcount records the cost on each usage event when it arrives: its
quantity times the matching cost. A cost you save or change later applies
only to events that arrive after it. So save your costs before the usage
arrives.

- An event gets a cost only when it carries `provider`, `token_type` and a
  `model` (or `model_raw`) label. Other events, such as usage of a meter
  that is not for tokens, get none and are not counted.
- When a LiteLLM record has a cost breakdown, Hardcount can take LiteLLM's
  own cost for each kind of token instead of yours
  ([when](../integrations/litellm.md#things-to-know)).

## The margin report

`GET /v1/customers/{id}/margin?from=…&to=…` answers for one customer and a
time window. Left out, `to` is the customer's current time and `from` is
30 days before that time. On a test clock, that is the clock's time. A
window can be at most one year.

| Field | What it is |
|---|---|
| `revenue_cents` | What the window's usage earns at the customer's current prices. |
| `cost_cents` | The recorded cost of the window's events, as a decimal string in US cents. |
| `margin_bps` | (revenue − cost) ÷ revenue, in hundredths of a percent: 5,000 is 50%. |
| `by_model` | The same three figures for each model. |
| `unattributed_revenue_cents` | Revenue from prices that do not name a `model`, so it fits no model's row. |
| `unresolved_events` | Token events with no cost found. |

The **Margin** card on the customer's page in the dashboard shows the same
report, starting on the current billing period.

## Worked example: the quick start's demo

The [quick start](../quick-start.md) demo bills one customer for a month of
Claude tokens. Its saved costs and the month's usage:

| Model and tokens | Quantity | Cost per million | Cost |
|---|---|---|---|
| Sonnet 4.5 input | 400,000 | $1.20 | $0.48 |
| Sonnet 4.5 output | 175,000 | $4.50 | $0.7875 |
| Sonnet 4.5 cache read | 200,000 | $0.05 | $0.01 |
| Haiku 4.5 input | 600,000 | $0.80 | $0.48 |
| Haiku 4.5 output | 250,000 | $4.00 | $1.00 |

The invoice billed $3.88 for Sonnet ($1.20 + $2.62 + $0.06) and $1.85 for
Haiku ($0.60 + $1.25). So:

| | Revenue | Cost | Margin |
|---|---|---|---|
| Sonnet 4.5 | $3.88 | $0.48 + $0.7875 + $0.01 = $1.2775 | (3.88 − 1.2775) ÷ 3.88 = 67.1% |
| Haiku 4.5 | $1.85 | $0.48 + $1.00 = $1.48 | (1.85 − 1.48) ÷ 1.85 = 20.0% |
| Total | $5.73 | $2.7575 | (5.73 − 2.7575) ÷ 5.73 = 51.9% |

The report gives the total as `revenue_cents` 573, `cost_cents` "275.75"
and `margin_bps` 5188, since 297.25 ÷ 573 = 0.51876. The demo prints it
rounded: billed $5.73, cost $2.76, margin 51.9%.

## When `margin_bps` is left out

The report leaves `margin_bps` out, rather than show a wrong figure, when:

- **the revenue is not in USD.** Costs are in USD, so a plan priced in EUR
  or GBP shows no margin. Revenue in two currencies shows none either.
- **an event in the window has no cost.** `unresolved_events` counts them.
  Save the missing cost, for example for a provider such as `bedrock`.
- **no event in the window has a cost.** Save your costs first.
- **the revenue is 0.**

A row in `by_model` also leaves out its margin when it has revenue but no
recorded cost.

## What margin leaves out

- **Base fees, credit and tax.** Margin covers usage only.
- **What was billed.** Revenue is worked out again at each subscription's
  current prices, over active and trialing subscriptions. After a price
  change, or for a canceled subscription, it differs from the invoices.
- **Events with no cost labels.** Their price still counts in
  `revenue_cents`, so `margin_bps` reads higher than your real margin.
- **Claude Haiku 5.5's two prices.** A provider cost has no `context_tier`
  label, so one cost covers both of the model's prices. Your margin is
  wrong for one of them
  ([more](../ai-pricing.md#see-your-margin)).

More: every call, step by step:
[See your margin](../ai-pricing.md#see-your-margin).

**Next:** [Bill for AI tokens by model](../guides/bill-for-ai-tokens.md).
