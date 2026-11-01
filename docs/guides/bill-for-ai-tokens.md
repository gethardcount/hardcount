# Bill for AI tokens by model

This guide sets up per-token prices for each AI model and kind of token,
then turns one provider response into usage events. At the end you know
what the invoice will charge. It is for a developer whose app calls
Anthropic or OpenAI and resells that access.

Using a LiteLLM proxy? It sends the events for you: follow
[LiteLLM integration](../integrations/litellm.md) after step 1.

## Words on this page

| Word | Meaning |
|---|---|
| Recipe | A ready-made pricing setup that one API call creates: a meter, a plan and its prices. |
| `token_type` | The kind of token: `input`, `output`, `cache_read`, `cache_write_5m` or `cache_write_1h`. Each has its own price. |
| Cache read and cache write | Prompt caching: tokens the provider reads from its cache cost less than input, and tokens it writes to the cache cost more. |

## Before you start

You need Hardcount running and a test secret key in `HARDCOUNT` and
`HARDCOUNT_KEY`, as in [Send usage and get your first
invoice](../first-invoice.md#before-you-start). That page also creates a
customer on a test clock. This guide uses its `CUSTOMER_REF` (the
customer's external ID) and `CUSTOMER_ID`.

## 1. Load ready-made prices

Apply the recipe for your provider. `anthropic_style` prices every Claude
model; `openai_style` prices the GPT, o-series and embedding models;
`replicate_style` prices GPU seconds.

```bash
curl -s -X POST "$HARDCOUNT/v1/recipes/anthropic_style/instantiate" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d '{}' | jq -r '.created_objects.plan_ids[0]'
```

**You see:** the plan ID, such as `hc_pln_…`.

The recipe creates a `tokens` meter, if you have none, and a monthly plan
named "AI API" with no base fee. The plan has one price for each model and
kind of token, at the provider's list prices. Applying it again returns the
same setup. The models, and how to set the plan's name and currency:
[Ready-made recipes](../ai-pricing.md#ready-made-recipes).

A customer must be on the plan before their usage is billed. The [first
invoice page](../first-invoice.md#steps), steps 2 to 4, creates the
customer and the subscription.

## 2. Turn a provider's token counts into events

Send one usage event for each kind of token in a call. Each event names the
model and the kind of token in its `dimensions`, so the plan's prices can
pick it. Give each event its own idempotency key, made from your call's ID
and the kind of token, so a resend counts once.

Providers report their counts differently, so map them with care:

| Provider field | `token_type` | Note |
|---|---|---|
| Anthropic `usage.input_tokens` | `input` | Excludes cache reads and writes. |
| Anthropic `usage.cache_read_input_tokens` | `cache_read` | |
| Anthropic `usage.cache_creation.ephemeral_5m_input_tokens` | `cache_write_5m` | Writes kept for 5 minutes. |
| Anthropic `usage.cache_creation.ephemeral_1h_input_tokens` | `cache_write_1h` | Writes kept for 1 hour. |
| Anthropic `usage.output_tokens` | `output` | |
| OpenAI `usage.prompt_tokens` minus `usage.prompt_tokens_details.cached_tokens` | `input` | `prompt_tokens` includes the cached tokens, so take them out. |
| OpenAI `usage.prompt_tokens_details.cached_tokens` | `cache_read` | |
| OpenAI `usage.completion_tokens` | `output` | |

Send the model name exactly as the recipe prices it, such as
`claude-sonnet-4.5` or `gpt-4o`, not the dated API name.

This shell function sends one event. It needs `CUSTOMER_REF` from the first
invoice page:

```bash
send_tokens() {   # send_tokens <call id> <token type> <quantity>
  jq -n --arg ref "$CUSTOMER_REF" --arg call "$1" --arg type "$2" --arg qty "$3" \
      '{event_name: "tokens", external_customer_id: $ref, quantity: $qty,
        idempotency_key: ($call + ":" + $type),
        dimensions: {provider: "anthropic", model: "claude-sonnet-4.5", token_type: $type}}' \
    | curl -s -X POST "$HARDCOUNT/v1/usage-events" \
      -H "Authorization: Bearer $HARDCOUNT_KEY" \
      -H "Content-Type: application/json" \
      -d @- | jq -c '{quantity, token_type: .dimensions.token_type}'
}
```

A Claude Sonnet 4.5 call answered with this usage:

```json
"usage": {
  "input_tokens": 2000,
  "cache_read_input_tokens": 50000,
  "cache_creation_input_tokens": 10000,
  "cache_creation": {"ephemeral_5m_input_tokens": 10000, "ephemeral_1h_input_tokens": 0},
  "output_tokens": 1500
}
```

becomes four events. A kind with 0 tokens needs no event.

```bash
send_tokens call-1 input 2000
send_tokens call-1 cache_read 50000
send_tokens call-1 cache_write_5m 10000
send_tokens call-1 output 1500
```

**You see:** one line for each event:

```text
{"quantity":"2000","token_type":"input"}
{"quantity":"50000","token_type":"cache_read"}
{"quantity":"10000","token_type":"cache_write_5m"}
{"quantity":"1500","token_type":"output"}
```

`provider` is not used by the recipe's prices, but your margin needs it to
find your cost ([Margin](../concepts/margin.md)).

## 3. Claude Haiku 5.5: the prompt-length price

Claude Haiku 5.5 has two prices. A call whose prompt is over 100,000 tokens
pays the higher one, for every token of the call, output included. The
prompt counts input, cache reads and cache writes.

The recipe's Haiku 5.5 prices pick events by one more label,
`context_tier`. Work it out once per call, and put it on every event of
that call:

- prompt = input + cache reads + cache writes;
- over 100,000: `"context_tier": "prompt_over_100k"`;
- 100,000 or less: `"context_tier": "prompt_up_to_100k"`.

For example, a call with 90,000 input tokens and 20,000 cache reads has a
prompt of 110,000 tokens. All its events, its output too, carry
`prompt_over_100k`. A Haiku 5.5 event without `context_tier` fits no
recipe price, so it is not billed.

A LiteLLM proxy adds `context_tier` for you.

## 4. What the invoice charges

At the end of the period, each model and kind of token becomes one invoice
line, such as "Tokens (claude-sonnet-4.5 · input)". Its quantity is the
period's total for that kind; the amount is rounded to whole cents once.

Say the customer made 1,000 calls like the one in step 2 in June. With the
recipe's Claude Sonnet 4.5 prices:

| Line | Quantity | Price per million | Amount |
|---|---|---|---|
| input | 2,000,000 | $3.00 | 2,000,000 × 0.0003 cents = $6.00 |
| cache_read | 50,000,000 | $0.30 | 50,000,000 × 0.00003 cents = $15.00 |
| cache_write_5m | 10,000,000 | $3.75 | 10,000,000 × 0.000375 cents = $37.50 |
| output | 1,500,000 | $15.00 | 1,500,000 × 0.0015 cents = $22.50 |

The invoice total is $6.00 + $15.00 + $37.50 + $22.50 = $81.00, before tax
and credit. To see the invoice on a test clock now, move the clock past
the period end, as in [first invoice](../first-invoice.md#steps) steps 6
and 7.

## 5. Price a model the recipe does not cover

Usage of a model with no price is stored and not billed: the event's answer
carries `billing_status: "no_price"`. Add a price for it to the plan with
`POST /v1/plans/{id}/prices`, one for each kind of token. It applies from
each subscriber's next billing period.

Every call: [Price a model a recipe does not
cover](../ai-pricing.md#price-a-model-a-recipe-does-not-cover).

## If something goes wrong

| What you see | What to do |
|---|---|
| `meter "tokens" not found` | Step 1 did not run. Apply the recipe. |
| `customer "…" not found` | `CUSTOMER_REF` is not a customer's `external_id`. Create the customer first. |
| `billing_status: "no_price"` on an event | No price takes its `model` and `token_type`. Check the model name against the recipe, or add a price (step 5). |
| `billing_status: "not_on_plan"` | The customer's plan has no price on the `tokens` meter. Put the customer on the recipe's plan. |
| `billing_status: "outside_subscription"` | The customer has no subscription that bills the event's time. |

**Next:** see what the usage cost you: [Margin](../concepts/margin.md).
More: [How Hardcount picks the price for an
event](../ai-pricing.md#how-hardcount-picks-the-price-for-an-event).
