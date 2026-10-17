# AI pricing reference

This page is the reference for pricing AI usage in Velox. It lists the
models each ready-made recipe prices. It shows how to price another model,
how Velox picks the price for each usage event, how to sell prepaid
credit, and how to read your margin.

**New to Velox?** Start with [Send usage and get your first
invoice](first-invoice.md). It runs every call in order. This page assumes
you did it, so `VELOX`, `VELOX_KEY` and `CUSTOMER_ID` are set in your
terminal.

Think of one `tokens` meter as a single counter with a price list beside
it. Each line on the list is a price for one model and one kind of token.

## Words on this page

| Word | Meaning |
|---|---|
| Meter | A counter for one kind of usage, such as AI tokens. |
| Dimensions | Labels on a usage event, such as `model` and `token_type`. |
| Rating rule | A price: an amount per unit, tiers, or packages. |
| Pricing rule | A line on a meter that says which events a rating rule prices, by their dimensions. |
| Default price | The price a meter uses for events that no pricing rule picks. |
| Prepaid commit | Usage credit the customer pays for up front, often at a discount. Later invoices draw it down. |
| Provider cost | What your AI provider charges you per token. |
| Margin | What you billed a customer, minus what their usage cost you, as a percent of what you billed. |

## Ready-made recipes

A recipe creates a meter, a price for each model and kind of token, and a
monthly plan with no base fee, in one call. It also creates a schedule for
retrying failed card payments, and a webhook endpoint (a URL in your app
that Velox calls when something happens). The endpoint starts turned off,
with a placeholder address: set your own address and turn it on under
**Webhooks**. The prices start at the provider's public list prices, which you can change
later.

| Recipe | Meter | Models it prices | Kinds of token (`token_type`) |
|---|---|---|---|
| `anthropic_style` | `tokens` | `claude-fable-5`, `claude-mythos-5`, `claude-opus-5`, `claude-sonnet-5`, `claude-opus-4.8`, `claude-opus-4.7`, `claude-opus-4.6`, `claude-opus-4.5`, `claude-opus-4.1`, `claude-opus-4`, `claude-sonnet-4.6`, `claude-sonnet-4.5`, `claude-sonnet-4`, `claude-haiku-4.5`, `claude-3.5-sonnet`, `claude-3.5-haiku`, `claude-3-opus`, `claude-3-sonnet`, `claude-3-haiku` | `input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h` |
| `openai_style` | `tokens` | `gpt-5.6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.5`, `gpt-5.5-pro`, `gpt-5.4`, `gpt-5.4-pro`, `gpt-5.4-mini`, `gpt-5.4-nano`, `gpt-5.3-codex`, `gpt-5.1`, `gpt-5`, `gpt-5-mini`, `gpt-5-nano`, `gpt-4.1`, `gpt-4.1-mini`, `gpt-4.1-nano`, `gpt-4o`, `gpt-4o-mini`, `gpt-4-turbo`, `gpt-3.5-turbo`, `o1`, `o1-pro`, `o1-mini`, `o3`, `o3-pro`, `o3-mini`, `o4-mini`, `text-embedding-3-large`, `text-embedding-3-small`, `text-embedding-ada-002` | `input`, `output`, and `cache_read` where the model has it |
| `replicate_style` | `gpu_seconds` | GPU time, by `gpu_type`: `h100`, `a100`, `a40`, `t4`, `cpu` | (none: one price per second for each GPU type) |

Send the model name exactly as the table shows it, in the `model`
dimension. A LiteLLM proxy does this for you: Velox turns a name such as
`claude-sonnet-4-5-20250929` into `claude-sonnet-4.5`.

The two AI recipes share the `tokens` meter, so you can apply both. Each
adds its own plan. All prices on one meter must be in one currency, so
apply the second recipe in the currency of the first.

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

You need two calls: one makes the price, and one tells the meter which
events it prices.

1. **Make the price.** This is a rating rule. `flat_amount_cents` is the
   price of one unit in cents, and can be a fraction: `0.0002` cents per
   token is $2.00 per million tokens.

   ```bash
   RATE_ID=$(curl -s -X POST "$VELOX/v1/rating-rules" \
     -H "Authorization: Bearer $VELOX_KEY" \
     -H "Content-Type: application/json" \
     -d '{"rule_key": "my_model_input", "name": "My model input", "mode": "flat",
          "currency": "USD", "flat_amount_cents": "0.0002"}' | jq -r .id)
   echo "$RATE_ID"
   ```

   **You see:** the price ID, such as `vlx_rrv_…`.

2. **Find the `tokens` meter.**

   ```bash
   METER_ID=$(curl -s "$VELOX/v1/meters" -H "Authorization: Bearer $VELOX_KEY" \
     | jq -r '.data[] | select(.key == "tokens") | .id')
   echo "$METER_ID"
   ```

   **You see:** the meter ID, such as `vlx_mtr_…`.

3. **Add a pricing rule to the meter.** It prices the events whose
   dimensions match `dimension_match`.

   ```bash
   jq -n --arg rate "$RATE_ID" \
       '{dimension_match: {model: "my-model", token_type: "input"},
         rating_rule_version_id: $rate, aggregation_mode: "sum", priority: 100}' \
     | curl -s -X POST "$VELOX/v1/meters/$METER_ID/pricing-rules" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq '{id, dimension_match, priority}'
   ```

   **You see:**

   ```json
   {
     "id": "vlx_mpr_…",
     "dimension_match": {
       "model": "my-model",
       "token_type": "input"
     },
     "priority": 100
   }
   ```

Every plan that charges for the `tokens` meter now prices `my-model` input
tokens. Add one rule for each kind of token you sell.

## How Velox picks the price for an event

- **One price for each event.** Each event is priced by at most one pricing
  rule: the one with the highest priority among the rules that match. So
  nothing is counted twice.
- **One rule for each model and kind of token.** Kinds of token do not
  overlap, so with one rule for each `{model, token_type}`, every event
  matches exactly one rule. This is what the recipes do.
- **A broad rule and finer rules together.** You can add a rule for a whole
  model (`{"model": "my-model"}`) next to rules for each kind of token.
  Priority decides which rule prices each event.

**Usage that no rule picks has no price.** The usual cause is a new model
name. If the meter has a default price, that price bills it. To set one,
send `PATCH /v1/meters/{id}` with `rating_rule_version_id`. If the meter has
no default price, Velox makes and sends the invoice without that usage, as
Lago, Metronome and Orb do. You see it in four places:

- the "not billed" row of the customer's usage, on the customer page and
  in `GET /v1/customers/{id}/usage` (the current billing period, unless
  you pass `from` and `to`);
- the warning on the invoice preview, which gives the quantity;
- the `velox_usage_unpriced_quantity_total{meter_id, mode}` metric (usage
  of customers on a test clock is not counted);
- a server log line when the invoice is made.

To bill it, add a rule or a default price. Invoices not made yet then
include it. For a period that is already invoiced, bill it with a one-off
invoice ([runbook](ops/runbook.md#16-usage-with-no-price)). Velox refuses
(409) to remove a meter's last price while a plan with a live subscription
uses the meter.

## Sell prepaid credit (a prepaid commit)

A prepaid commit is a one-off invoice with a special line. When the invoice
is finalized (issued), the customer gets the credit, and later invoices
draw it down. Here the customer pays $9,000 for $10,000 of credit.

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
     -H "Authorization: Bearer $VELOX_KEY" | jq -c '{balance_cents, total_granted}'
   ```

   **You see:** `{"balance_cents":1000000,"total_granted":1000000}`

One invoice can hold at most one commit line. To stop a customer's usage
when the credit runs out, see [Stop usage when credit runs
out](stop-usage-when-credit-runs-out.md).

## See your margin

Your margin on a customer is what you billed them, against what their
usage cost you. Only you see it, never the customer.

1. **Save what your provider charges you per token.** Do this before the
   usage arrives. Velox records the cost on each event when it arrives, so
   a cost you change later applies only to later events. Save one cost for
   each model and kind of token. These are made-up costs for Claude Sonnet
   4.5: $1.20 per million input tokens and $4.50 per million output tokens.

   ```bash
   save_cost() {   # save_cost <token type> <cost per token in dollars>
     jq -n --arg type "$1" --arg cost "$2" \
         '{provider: "anthropic", model: "claude-sonnet-4.5",
           token_type: $type, cost_per_token: $cost}' \
       | curl -s -X PUT "$VELOX/v1/provider-costs" \
         -H "Authorization: Bearer $VELOX_KEY" \
         -H "Content-Type: application/json" \
         -d @- | jq -c '{id, token_type, cost_per_token}'
   }
   save_cost input 0.0000012
   save_cost output 0.0000045
   ```

   **You see:**

   ```text
   {"id":"vlx_pcr_…","token_type":"input","cost_per_token":"0.0000012"}
   {"id":"vlx_pcr_…","token_type":"output","cost_per_token":"0.0000045"}
   ```

   The **Provider costs** page lists them.

2. **Read the margin for one invoiced period.** This reads the billing
   period of the invoice in `INVOICE_ID`.

   ```bash
   PERIOD=$(curl -s "$VELOX/v1/invoices/$INVOICE_ID" \
     -H "Authorization: Bearer $VELOX_KEY" \
     | jq -r '.invoice | "from=\(.billing_period_start | @uri)&to=\(.billing_period_end | @uri)"')
   curl -s "$VELOX/v1/customers/$CUSTOMER_ID/margin?$PERIOD" \
     -H "Authorization: Bearer $VELOX_KEY" | jq '{revenue_cents, cost_micros, margin_bps}'
   ```

   **You see,** for a [first invoice](first-invoice.md) made after you
   saved these two costs:

   ```json
   {
     "revenue_cents": 600,
     "cost_micros": 2100000,
     "margin_bps": 6500
   }
   ```

   Billed $6.00, cost $2.10 (`cost_micros` is in millionths of a dollar),
   and a 65% margin (`margin_bps` is in hundredths of a percent).

Without `from` and `to`, the margin covers the last 30 days of the
customer's time. On a test clock, that is the clock's time. The
**Margin** card on the customer page starts on the current billing period.

## Usage from a LiteLLM proxy

A LiteLLM proxy can send each AI call to Velox for you, with no code. Each
call becomes token events on the `tokens` meter. LiteLLM's `user` must be
the customer's external ID. Cache-write tokens have no `token_type` of
their own from LiteLLM yet: for Anthropic and Bedrock, LiteLLM counts them
as input tokens, so they are billed at your input price. See [LiteLLM
integration](integrations/litellm.md).

Related: [the design of multi-dimension meters](design-multi-dim-meters.md).
