# AI pricing reference

**New to Velox?** Start with [Send usage and get your first
invoice](first-invoice.md). It runs every call in order. This page assumes
you did it, so `VELOX`, `VELOX_KEY` and `CUSTOMER_ID` are set in your
terminal.

## Words on this page

| Word | Meaning |
|---|---|
| Dimensions | Labels on a usage event, such as `model` and `token_type`. |
| Price | One usage rate of a plan: a meter, the labels an event must carry (`dimension_match`), and an amount per unit, tiers, or packages. |
| Used plan | A plan a subscription has used. A price change on it applies from each subscriber's next billing period. A subscriber who joins the plan after the change pays it from the join, unless the same subscription was on the plan earlier in that period. |

## Ready-made recipes

A recipe creates, in one call, a monthly plan with no base fee and its
prices: one for each model and kind of token, or for each GPU type in
`replicate_style`. The prices bill the meter in the table below. If you
have no meter with that event name, the recipe creates it. If you have one
with the same aggregation, the recipe uses it. If yours has another
aggregation, the recipe is refused with 409 and creates nothing. It creates
nothing else: set payment retries under **Dunning policies** and webhook
endpoints under **Webhooks**. The prices start at the provider's public
list prices, which you can change later.

| Recipe | Meter | Models it prices | Kinds of token (`token_type`) |
|---|---|---|---|
| `anthropic_style` | `tokens` | `claude-fable-5.1`, `claude-mythos-5.1`, `claude-opus-5.5`, `claude-sonnet-5.5`, `claude-haiku-5.5` (two prices, by `context_tier`), `claude-fable-5`, `claude-mythos-5`, `claude-opus-5`, `claude-sonnet-5`, `claude-opus-4.8`, `claude-opus-4.7`, `claude-opus-4.6`, `claude-opus-4.5`, `claude-opus-4.1`, `claude-opus-4`, `claude-sonnet-4.6`, `claude-sonnet-4.5`, `claude-sonnet-4`, `claude-haiku-4.5`, `claude-3.5-sonnet`, `claude-3.5-haiku`, `claude-3-opus`, `claude-3-sonnet`, `claude-3-haiku` | `input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h` |
| `openai_style` | `tokens` | `gpt-5.6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.5`, `gpt-5.5-pro`, `gpt-5.4`, `gpt-5.4-pro`, `gpt-5.4-mini`, `gpt-5.4-nano`, `gpt-5.3-codex`, `gpt-5.1`, `gpt-5`, `gpt-5-mini`, `gpt-5-nano`, `gpt-4.1`, `gpt-4.1-mini`, `gpt-4.1-nano`, `gpt-4o`, `gpt-4o-mini`, `gpt-4-turbo`, `gpt-3.5-turbo`, `o1`, `o1-pro`, `o1-mini`, `o3`, `o3-pro`, `o3-mini`, `o4-mini`, `text-embedding-3-large`, `text-embedding-3-small`, `text-embedding-ada-002` | `input`, `output`, and `cache_read` where the model has it |
| `replicate_style` | `gpu_seconds` | GPU time, by `gpu_type`: `h100`, `a100`, `a40`, `t4`, `cpu` | (none: one price per second for each GPU type) |

Send the model name exactly as the table shows it, in the `model`
dimension. A LiteLLM proxy does this for you: Velox turns a name such as
`claude-sonnet-4-5-20250929` into `claude-sonnet-4.5`.

- **A Claude name matches a model only when it is the model's id, or the id
  with a date or version ending,** such as `-20250929`, `-v1:0`,
  `@20250805` or `@default`. Bedrock, Google Cloud and Foundry prefixes are
  removed first. A name Velox does not know, such as `claude-opus-4-9`, is
  never priced as an older model. It keeps its own name and has no price
  until you add one ([how](#price-a-claude-model-velox-does-not-know)).
- **Claude Haiku 5.5 has two prices.** A call whose prompt is over 100,000
  tokens pays the higher one, for every token of the call, output included.
  The prompt counts input, cache reads and cache writes. Its events carry
  `context_tier`: `prompt_up_to_100k` or `prompt_over_100k`. The recipe has
  a price for each. If you price Haiku 5.5 yourself, put `context_tier` in
  every price: a price without it prices both at one rate. A provider cost
  has no `context_tier`, so your margin on Haiku 5.5 is wrong for one of
  the two prices ([more](#see-your-margin)).
- **The prices are Anthropic's list prices.** On Bedrock and Google Cloud,
  a regional endpoint costs you 10% more than global for Claude 4.5 and
  later models; the recipe charges the same for both. Fast mode, US-only
  inference and batch prices are not applied.

The two AI recipes share the `tokens` meter, so you can apply both. Each
adds its own plan with its own prices, in the plan's currency.

To see the recipes, and to apply one with your own plan name:

```bash
curl -s "$VELOX/v1/recipes" -H "Authorization: Bearer $VELOX_KEY" \
  | jq -c '[.data[] | {key, name}]'
curl -s -X POST "$VELOX/v1/recipes/openai_style/instantiate" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"overrides": {"plan_name": "GPT API"}}' \
  | jq -c '{recipe_key, plans: .created_objects.plan_ids}'
```

**You see:** the three recipes, then the recipe you applied and its new
plan:

```text
[{"key":"anthropic_style","name":"Anthropic (Claude) token pricing"},{"key":"openai_style","name":"OpenAI (GPT) token pricing"},{"key":"replicate_style","name":"GPU compute pricing (per-second)"}]
{"recipe_key":"openai_style","plans":["vlx_pln_…"]}
```

The overrides are `currency` (`USD`, `EUR` or `GBP`), `plan_name` and
`plan_code`. Applying a recipe a second time returns the setup it made
the first time, so the overrides count only on the first apply.

## Price a model a recipe does not cover

Add a price to the plan. It names the meter, the labels an event must carry
(`dimension_match`), and the rate. `mode: "per_unit"` bills each unit at
`unit_amount_cents`, the price of one unit in cents, which can be a
fraction: `0.0002` cents per token is $2.00 per million tokens. The price
takes the plan's currency.

1. **Find the `tokens` meter and your plan.**

   ```bash
   METER_ID=$(curl -s "$VELOX/v1/meters" -H "Authorization: Bearer $VELOX_KEY" \
     | jq -r '.data[] | select(.event_name == "tokens") | .id')
   PLAN_ID=$(curl -s "$VELOX/v1/plans" -H "Authorization: Bearer $VELOX_KEY" \
     | jq -r '.data[] | select(.name == "GPT API") | .id')
   echo "$METER_ID $PLAN_ID"
   ```

   **You see:** the two IDs, such as `vlx_mtr_… vlx_pln_…`.

2. **Add the price.**

   ```bash
   jq -n --arg meter "$METER_ID" \
       '{meter_id: $meter, dimension_match: {model: "my-model", token_type: "input"},
         mode: "per_unit", unit_amount_cents: "0.0002"}' \
     | curl -s -X POST "$VELOX/v1/plans/$PLAN_ID/prices" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -c '{id, dimension_match, unit_amount_cents, effective}'
   ```

   **You see:**
   `{"id":"vlx_prc_…","dimension_match":{"model":"my-model","token_type":"input"},"unit_amount_cents":"0.0002","effective":"next_period"}`

`effective` says when it applies. On a plan no subscription has used it is
`now`. On a used plan it is `next_period`: each subscriber pays it from
their next billing period, and the period already open keeps the prices it
started with. A subscriber who joins the plan after the change pays it from
the moment they join ([Change a price](#change-a-price)). Add one price for
each kind of token you sell. Other plans that price the `tokens` meter are
not changed.

## How Velox picks the price for an event

- **One price for each event.** A price takes an event whose labels include
  every label of the price. When several prices of the plan take an event,
  the one with the most labels bills it. So nothing is counted twice.
- **One price for each model and kind of token.** Kinds of token do not
  overlap, so with one price for each `{model, token_type}`, every event
  fits exactly one price. This is what the recipes do.
- **A broad price and finer prices together.** A price for a whole model
  (`{"model": "my-model"}`) next to prices for each kind of token bills the
  kinds of token that have no price of their own. A price with no labels
  takes every event no other price takes.
- **Two prices that could tie are refused.** Two prices with the same
  number of labels that could both take one event, such as
  `{"model": "a"}` and `{"token_type": "input"}`, have no winner. The second
  one is refused with 409 `price_conflict`, which names the first. Add a
  label to one of them, or give a shared label another value.

**Usage that no price takes is not billed.** The usual cause is a new model
name. Velox makes and sends the invoice without that usage. You see it in
four places:

- the "not billed" row of the customer's usage, on the customer page and
  in `GET /v1/customers/{id}/usage` (the current billing period, unless
  you pass `from` and `to`), marked `no_price`;
- the warning on the invoice preview, which gives the quantity;
- the `velox_usage_unpriced_quantity_total{meter_id, mode}` metric (usage
  of customers on a test clock is not counted);
- a server log line when the invoice is made.

To bill it from the next period, add a price. For usage already left out,
bill it with a one-off invoice
([runbook](ops/runbook/usage-with-no-price.md)).

## Change a price

A price's rate changes with `PATCH /v1/plans/{id}/prices/{price_id}`, and
`DELETE` removes it. Its meter and labels never change: for other events,
add another price. Each billing period of a subscription pays the plan's
prices as they were at the period's start. If the subscription joined the
plan during the period, it pays them as they were when it first joined the
plan in that period. So:

- On a plan no subscription has used, a change applies at once
  (`effective: now`).
- On a used plan, it applies from each subscriber's next billing period
  (`effective: next_period`). The period already open keeps the prices it
  started with, also after a quantity change. A customer on a test clock
  follows the clock: the change applies from the clock's next period.
- A subscriber who joins the plan after a change pays it at once: a new
  subscription from its start (after a trial, from the trial's end), an
  immediate plan change onto the plan from the change, an added item from
  the add. So a subscription that starts later on the day of a change pays
  the new rate, bills a price added earlier and does not bill a price
  removed earlier, from its first period.
- A customer's negotiated rate (a price override) keeps applying when the
  price's rate changes. Removing the price ends it, and the answer lists the
  overrides it ended.
- A subscription that leaves the plan and joins it again in the same
  period keeps the prices of its first join for that period. That holds for
  an item removed and added back, and for a plan change and back. A change
  made in between applies from its next period.
- An override follows the same rule. It applies from the subscriber's next
  period, or from the join when the subscription first joins the plan in a
  period after the override was created. So one created before a
  subscription starts (after a trial, before the trial ends), even on the
  same day, applies from its first period.
- When an item leaves a plan and an item joins it again later in the
  period, each part bills on its own, with its own peak and tiers. An item
  added on the period's first day bills from the period start, so its part
  starts where the part before it ended. On one plan those two parts bill
  as one: a `max` meter bills one peak, and tiers run on.
- On a test clock, a change or an override takes effect just after the
  clock's current time. A subscription, an immediate plan change or an
  added item at that same clock time gets the earlier prices, so advance
  the clock first.

On the dashboard, open the plan under **Pricing**. Its **Usage prices**
card says which of the two applies before you save.

The meter's aggregation has its own rule: it can change until the meter has
a usage event, then it is fixed (409 `meter_has_usage`). Held usage does not
count until it is released. Every price of the meter bills under it.

## Price a Claude model Velox does not know

A Claude name Velox does not know keeps its own name in `model`, such as
`claude-opus-4-9`. Price it as in [Price a model a recipe does not
cover](#price-a-model-a-recipe-does-not-cover), with one price for each kind
of token. This example adds it to the `anthropic_style` plan "AI API":

```bash
CLAUDE_PLAN_ID=$(curl -s "$VELOX/v1/plans" -H "Authorization: Bearer $VELOX_KEY" \
  | jq -r '.data[] | select(.name == "AI API") | .id')
jq -n --arg meter "$METER_ID" \
    '{meter_id: $meter, dimension_match: {model: "claude-opus-4-9", token_type: "input"},
      mode: "per_unit", unit_amount_cents: "0.0015"}' \
  | curl -s -X POST "$VELOX/v1/plans/$CLAUDE_PLAN_ID/prices" \
    -H "Authorization: Bearer $VELOX_KEY" \
    -H "Content-Type: application/json" \
    -d @- | jq -c '{dimension_match, effective}'
```

**You see,** on a plan a subscription has used (on an unused plan,
`effective` is `now`):
`{"dimension_match":{"model":"claude-opus-4-9","token_type":"input"},"effective":"next_period"}`

If a later release learns the model and renames it, the price stops
matching, and the usage shows as not billed until you price the new name.

## Price one platform's model name differently

Each event keeps the name LiteLLM sent in `model_raw`, such as
`vertex_ai/claude-opus-4-6@default`. To charge that name another rate,
add a price with one more label than the recipe's price for the model:
`model_raw` beside its labels. The price with more labels wins. This
example charges Claude Opus 4.6 input tokens sent under that name $5.50
per million, on the `anthropic_style` plan "AI API":

```bash
CLAUDE_PLAN_ID=$(curl -s "$VELOX/v1/plans" -H "Authorization: Bearer $VELOX_KEY" \
  | jq -r '.data[] | select(.name == "AI API") | .id')
jq -n --arg meter "$METER_ID" \
    '{meter_id: $meter,
      dimension_match: {model: "claude-opus-4.6", model_raw: "vertex_ai/claude-opus-4-6@default", token_type: "input"},
      mode: "per_unit", unit_amount_cents: "0.00055"}' \
  | curl -s -X POST "$VELOX/v1/plans/$CLAUDE_PLAN_ID/prices" \
    -H "Authorization: Bearer $VELOX_KEY" \
    -H "Content-Type: application/json" \
    -d @- | jq -c '{dimension_match, effective}'
```

**You see,** on a plan a subscription has used (on an unused plan,
`effective` is `now`):
`{"dimension_match":{"model":"claude-opus-4.6","model_raw":"vertex_ai/claude-opus-4-6@default","token_type":"input"},"effective":"next_period"}`

- Keep `model` in the price. `{model_raw, token_type}` beside the recipe's
  `{model, token_type}` has as many labels, and both could take one event,
  so it is refused with 409 `price_conflict`.
- Claude Haiku 5.5's recipe prices also carry `context_tier`, so a price
  for one of its names needs four labels: `model`, `model_raw`,
  `context_tier` and `token_type`.

## Sell prepaid credit (a prepaid commit)

A prepaid commit is a one-off invoice with a special line. When the invoice
is finalized (issued), the customer gets the credit, and later invoices
in the same currency draw it down. Here the customer pays $9,000 for
$10,000 of credit.

1. **Make a one-off invoice with the commit line.** `unit_amount_cents` is
   what the customer pays. `commit_granted_cents` is the credit they get.

   ```bash
   COMMIT_INVOICE_ID=$(jq -n --arg cus "$CUSTOMER_ID" \
       '{customer_id: $cus, line_items: [{description: "Annual commit",
         line_type: "add_on", quantity: 1, unit_amount_cents: 900000,
         commit_granted_cents: 1000000}]}' \
     | curl -s -X POST "$VELOX/v1/invoices" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .id)
   echo "$COMMIT_INVOICE_ID"
   ```

   **You see:** the invoice ID, such as `vlx_inv_…`. The invoice is a
   draft.

2. **Finalize it.**

   ```bash
   curl -s -X POST "$VELOX/v1/invoices/$COMMIT_INVOICE_ID/finalize" \
     -H "Authorization: Bearer $VELOX_KEY" | jq -c '{status, total_amount_cents}'
   ```

   **You see:** `{"status":"finalized","total_amount_cents":900000}`

3. **Check the credit balance.**

   ```bash
   curl -s "$VELOX/v1/credits/balance/$CUSTOMER_ID" \
     -H "Authorization: Bearer $VELOX_KEY" | jq -c '.data[] | {currency, balance_cents, total_granted}'
   ```

   **You see:** `{"currency":"USD","balance_cents":1000000,"total_granted":1000000}`
   (one line per currency the customer holds credit in).

One invoice can hold at most one commit line. To stop a customer's usage
when the credit runs out, see [Stop usage when credit runs
out](stop-usage-when-credit-runs-out.md).

### Tax on prepaid credit

With a tax provider on, Velox charges tax once: on the usage, not on the
credit purchase. It treats buying credit as an advance payment. The usage the credit pays for is taxed when it is invoiced.

At 10% tax, the commit above is invoiced at $9,000 with no tax. A later
usage invoice of $1,000 carries $100 of tax, and the credit pays all $1,100
of it. So with tax on, the credit covers less usage than its face value:
the $10,000 of credit pays for about $9,091 of usage and $909 of tax.

- **You see:** the commit line has tax 0 and `"tax_reason":
  "prepaid_credit_purchase"`. The dashboard marks it "Prepaid credit, not
  taxed", and the PDF says why there is no tax.
- **Refunds:** refunding unused credit
  ([ADR-080](adr/080-paid-commit-cn-relief.md)) reverses no tax, because the
  purchase carried none. A commit whose purchase was taxed (invoiced before
  [#1288](https://github.com/getvelox/velox/pull/1288)) still reverses that
  tax when refunded.
- **Free (promotional) credit** pays usage invoices the same way, tax
  included.

Tax rules for prepayments differ by country. Some tax a prepayment when it
is received, others when the service is used. Before you sell commits with
tax on, check with your tax adviser:

- whether your country taxes prepaid credit when it is bought or when it is
  used;
- whether that depends on what the credit can be spent on;
- if you sell credit below face value, as in the example, whether its use
  is taxed on the price paid. Velox taxes the usage at its price, so the
  $9,000 commit above leads to tax on about $9,091 of usage;
- what your invoices must say about an untaxed prepayment.

Velox has no setting to tax the purchase instead. If your country requires
it, [open an issue](https://github.com/getvelox/velox/issues) before you
sell commits with tax on. The decision and its reasons are in
[ADR-078](adr/078-prepaid-commits-phase-1.md#amendment-2026-10-19--the-purchase-is-not-taxed-the-usage-it-pays-for-is).

## See your margin

Your margin on a customer is the price of their usage rated in the window,
invoiced or not, against what that usage cost you. Only you see it, never
the customer.

1. **Save what your provider charges you per token.** Do this before the
   usage arrives. Velox records the cost on each event when it arrives, so
   a cost you change later applies only to later events. Save one cost for
   each model and kind of token. These are made-up costs for Claude Sonnet
   4.5: $1.20 per million input tokens and $4.50 per million output tokens.

   `model` must be the name the usage carries exactly, either `model` or
   `model_raw`. A cost on `claude-opus-5` does not cover
   `claude-opus-5.5`.

   A cost has no `context_tier`. So one Claude Haiku 5.5 cost covers both
   of its prices, and the margin is wrong for one of them. At Anthropic's
   list prices, a prompt over 100,000 tokens costs 5 times as much: if you
   save the cost of shorter prompts, the margin on longer ones reads too
   high. When a LiteLLM record has `cost_breakdown`, Velox can use
   LiteLLM's cost instead of yours
   ([when](integrations/litellm.md#things-to-know)).

   A cost is `unit_cost_cents`, US cents per token, like every `*_cents`
   field: `0.00012` cents per token is $1.20 per million tokens.

   ```bash
   save_cost() {   # save_cost <token type> <cost per token in cents>
     jq -n --arg type "$1" --arg cost "$2" \
         '{provider: "anthropic", model: "claude-sonnet-4.5",
           token_type: $type, unit_cost_cents: $cost}' \
       | curl -s -X PUT "$VELOX/v1/provider-costs" \
         -H "Authorization: Bearer $VELOX_KEY" \
         -H "Content-Type: application/json" \
         -d @- | jq -c '{id, token_type, unit_cost_cents}'
   }
   save_cost input 0.00012
   save_cost output 0.00045
   ```

   **You see:**

   ```text
   {"id":"vlx_pcr_…","token_type":"input","unit_cost_cents":"0.00012"}
   {"id":"vlx_pcr_…","token_type":"output","unit_cost_cents":"0.00045"}
   ```

   The **Provider costs** page lists them.

2. **Read the margin for one invoiced period.** This reads the billing
   period of the invoice in `INVOICE_ID`.

   ```bash
   PERIOD=$(curl -s "$VELOX/v1/invoices/$INVOICE_ID" \
     -H "Authorization: Bearer $VELOX_KEY" \
     | jq -r '.invoice | "from=\(.billing_period_start | @uri)&to=\(.billing_period_end | @uri)"')
   curl -s "$VELOX/v1/customers/$CUSTOMER_ID/margin?$PERIOD" \
     -H "Authorization: Bearer $VELOX_KEY" | jq '{revenue_cents, cost_cents, margin_bps}'
   ```

   **You see,** for a [first invoice](first-invoice.md) made after you
   saved these two costs:

   ```json
   {
     "revenue_cents": 600,
     "cost_cents": "210",
     "margin_bps": 6500
   }
   ```

   Billed $6.00, cost $2.10 (`cost_cents` is in cents, as a decimal string),
   and a 65% margin (`margin_bps` is in hundredths of a percent).

The margin covers usage only: base fees, credits and taxes are left out.
Revenue Velox cannot match to a model is shown apart, in
`unattributed_revenue_cents`. A token event that names its `provider`,
`token_type` and `model` but matches no cost rate is counted in
`unresolved_events`. So is an event whose cost, quantity ×
`unit_cost_cents`, is too large to record (about $9.2 trillion or more);
it is still billed. While any such event is in the window,
`margin_bps` is left out. An event sent through the usage API without one
of those three labels, such as usage on a meter that is not for tokens,
gets no cost and is not counted. Its price is still in `revenue_cents`, so
`margin_bps` reads higher than your real margin.

Without `from` and `to`, the margin covers the last 30 days of the
customer's time. On a test clock, that is the clock's time. The
**Margin** card on the customer page starts on the current billing period.

## Usage from a LiteLLM proxy

A LiteLLM proxy can send each AI call to Velox as token events on the
`tokens` meter. Setup, the `user` field, which calls are billed and how
cache tokens are billed: [LiteLLM integration](integrations/litellm.md).

Next: [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).
