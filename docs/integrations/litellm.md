# LiteLLM integration

LiteLLM is an open-source proxy that sits between your app and many AI
providers. This page connects it to Velox, so that your customers' AI calls
become usage in Velox. You add a few lines to the proxy's config file and
pass each customer's external ID as `user` on every call (step 3). Not
using LiteLLM? Send usage with the API instead: see [Send usage and get
your first invoice](../first-invoice.md).

## Before you start

Set these up before LiteLLM sends its first call. Usage that arrives
before the customer or the meter exists is held. Once released, usage for a
customer created after the calls is usually not invoiced, because it
predates the subscription
([held usage](#usage-for-a-customer-velox-does-not-know-yet)).

1. **Velox running, and a secret API key.** A publishable key cannot send
   usage. The commands below use the variables from [the first invoice
   page](../first-invoice.md#before-you-start):

   ```bash
   export VELOX=http://localhost:8080
   export VELOX_KEY=vlx_secret_test_…   # your test secret key
   ```

2. **A `tokens` meter with prices.** Step 1 below creates it.
3. **Each customer created in Velox, and on a plan.** The customer's
   external ID (your own ID for it) must be the `user` you pass to LiteLLM.
   The subscription puts the customer on a plan, so the usage lands on an
   invoice. [The first invoice page](../first-invoice.md), steps 2 to 4,
   shows both calls.

## 1. Load prices for your models

Apply a recipe: `anthropic_style` for Claude, `openai_style` for OpenAI
([what each creates and prices](../ai-pricing.md#ready-made-recipes)).

```bash
curl -s -X POST "$VELOX/v1/recipes/anthropic_style/instantiate" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{}' | jq -r '.created_objects.plan_ids[0]'
```

**You see:** the plan ID, such as `vlx_pln_…`.

You can also create the meter by hand. You then also need a price for each
model and kind of token
([how](../ai-pricing.md#price-a-model-a-recipe-does-not-cover)):

```bash
curl -s -X POST "$VELOX/v1/meters" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"event_name":"tokens","name":"Tokens","unit":"tokens","aggregation":"sum"}'
```

## 2. Add Velox to the proxy config

Add this to your `litellm_config.yaml`. It uses LiteLLM's `generic_api`
callback, which sends a record of each finished call to a web address.
Tested with LiteLLM 1.83.9.

```yaml
litellm_settings:
  callbacks: ["velox"]

callback_settings:
  velox:
    callback_type: generic_api
    endpoint: "https://<your-velox-host>/v1/integrations/litellm/spend"
    headers:
      Authorization: Bearer vlx_secret_test_…
    max_retries: 4
    retry_delay: 5
    timeout: 10
```

- **`endpoint`:** your Velox address. On your own computer it is
  `http://localhost:8080`. If LiteLLM runs in a Docker container and Velox
  does not, `localhost` points at the container itself. Use
  `http://host.docker.internal:8080` instead. On Linux, start the container
  with `--add-host=host.docker.internal:host-gateway` for that name to
  work.
- **`Authorization`:** a secret key, as in "Before you start".
- **`max_retries` and `retry_delay`:** keep these. LiteLLM's default is no
  retries, so one failed send loses the batch. Velox answers 503 when its
  database is briefly out of reach, such as during a restart. Four retries
  5, 10, 20 and 40 seconds apart cover that.

The older form (`success_callback: ["generic"]` with
`GENERIC_LOGGER_ENDPOINT` and `GENERIC_LOGGER_HEADERS`) still works on
current LiteLLM, but it is the legacy form. With either form, Velox
accepts a single record, a list of records (`json_array`), or
`{"events": [...]}`.

## 3. Set `user` on every call

Velox finds the customer by LiteLLM's `user` field: it must equal the
external ID of a customer you created in Velox. Set it on every call:

```python
import litellm

response = litellm.completion(
    model="claude-sonnet-4-5-20250929",
    messages=[{"role": "user", "content": "Hello"}],
    user="acme-1",   # the customer's external ID in Velox
    metadata={
        "team_id": "team_engineering",  # becomes a label on the usage
    },
)
```

The proxy logs that value as `end_user`, and Velox reads it from there. A
record you build by hand can send `user` instead.

A call without `user` is refused, and the refused row is lost
([Response](#response)).

### Clients that cannot set `user`: Claude Code and Codex

Claude Code (`/v1/messages`) and Codex-style clients (`/v1/responses`) do
not send `user`. Have them send the customer's external ID in the
`x-litellm-customer-id` header instead. LiteLLM logs it as the call's end
user, which is where Velox reads the customer. For Claude Code, set
`ANTHROPIC_CUSTOM_HEADERS="x-litellm-customer-id: acme-1"` where it runs.

Without the header:

- Claude Code calls arrive with Claude Code's own user ID (its
  `metadata.user_id`, which includes the session) as the customer. Velox
  holds them under that ID, so there is no customer to release them to.
- A call with no customer at all is refused, as above.

The client chooses this header, so whoever runs the client chooses the
customer it is billed to. Give the header only to clients you trust.

## 4. What lands in Velox

Each AI call becomes up to five usage events on the `tokens` meter. The
`token_type` label tells them apart, and no token is counted twice.
LiteLLM's `prompt_tokens` already includes the tokens read from and written
to the cache, so Velox takes those out of `input`.

| `token_type` | Quantity | Idempotency key |
|---|---|---|
| `input` | `prompt_tokens` minus the cache reads and cache writes | `<call>:input` |
| `cache_read` | `prompt_tokens_details.cached_tokens` or `cache_read_input_tokens`, if any | `<call>:cache_read` |
| `cache_write_5m` | cache writes with the 5-minute lifetime, if any | `<call>:cache_write_5m` |
| `cache_write_1h` | cache writes with the 1-hour lifetime, if any | `<call>:cache_write_1h` |
| `output` | `usage.completion_tokens` | `<call>:output` |

The idempotency key is a unique ID for each event. If LiteLLM sends the same
call again, Velox counts it once. Velox saves a call's events together, or
none of them.

`<call>` is LiteLLM's call ID, the same ID as the call's row in
LiteLLM's spend log, so a replay from that log counts each call once.
When the ID is longer than 240 characters (a `/v1/responses` ID can be
over 400), `<call>` is `sha256-` followed by the ID's SHA-256 in hex; chat
completion, text completion and embedding IDs are never changed. On a
streamed `/v1/messages` call LiteLLM uses the client's `x-litellm-call-id`
header as the call ID. Two calls that send the same value count once in
Velox, as in LiteLLM's spend log, so don't reuse that header.

Each event also carries these labels (dimensions):

- `model`: the model family name that prices are set on. Velox turns
  LiteLLM's model name into it, for example `claude-sonnet-4-5-20250929`
  becomes `claude-sonnet-4.5`. It first removes provider and route
  prefixes such as `bedrock/`, `converse/`, `vertex_ai/`, `azure_ai/` and
  `anthropic.`, and Bedrock cross-region prefixes (`global.`, `us.`, `eu.`,
  `apac.`, `jp.`, `au.`, `us-gov.`). So
  `bedrock/global.anthropic.claude-opus-5-5` and
  `vertex_ai/claude-opus-4-6@default` become `claude-opus-5.5` and
  `claude-opus-4.6`. A Claude name matches only the model's id, or the id
  with a date or version ending. A name Velox does not know, such as
  `claude-opus-4-9`, keeps its own name here, has no price until you add
  one, and is logged as a warning
  ([how to price it](../ai-pricing.md#price-a-claude-model-velox-does-not-know)).
- `model_raw`: the model name exactly as LiteLLM sent it.
- `context_tier`, on Claude Haiku 5.5 events only: `prompt_up_to_100k` or
  `prompt_over_100k`. Haiku 5.5 has a higher price when the prompt (input,
  cache reads and cache writes) is over 100,000 tokens. Provider costs have
  no `context_tier`, so one Haiku 5.5 cost covers both prices
  ([more](../ai-pricing.md#see-your-margin)).
- `provider`: the provider, from LiteLLM's `custom_llm_provider`.
- `team_id`, when the call has one in `metadata`.

Velox keeps the call ID only in the idempotency keys (`<call>`, above).
LiteLLM's `response_cost` and the call's other metadata
are not stored ([what cost is saved](#things-to-know)).

**Cache writes** (Anthropic prompt caching) cost more than input: 1.25
times for the 5-minute lifetime and 2 times for the 1-hour one. LiteLLM
1.83.9 reports how many write tokens used each lifetime, under
`metadata.usage_object.prompt_tokens_details.cache_creation_token_details`,
and Velox bills each lifetime as its own `token_type`. The
`anthropic_style` recipe prices both. If you set your own prices, add a
`cache_write_5m` and a `cache_write_1h` price for each model, or those
tokens are billed at $0 and show as unmatched usage.

If a record has cache writes but no usable split, Velox bills them as
`input`, below what the provider charges, and counts the call in
`velox_litellm_cache_write_unsplit_total`.

For the cost side (your margin), a call with only 5-minute writes takes
LiteLLM's own cost figures. A call with 1-hour writes takes its input and
write costs from your [provider cost rates](../ai-pricing.md#see-your-margin) instead:
LiteLLM 1.83.9's figures do not price the 1-hour writes correctly.

A call sent again that Velox first saved before this split (as one `input`
event with the writes inside) is saved the old way again, and a cache-write
row of it that Velox was holding is not billed when released, so its writes
are not billed twice.

A streamed Claude call (Claude Code streams) usually arrives without the
split: LiteLLM 1.83.9 keeps only the usage of the last streamed event,
which carries no split. Its cache writes bill as `input` and count in
`velox_litellm_cache_write_unsplit_total`. The same happens to a
`/v1/responses` call to a Claude model when the proxy has a `post_call`
guardrail. LiteLLM's record then has a cache-write cost but no cache-write
count, and the call counts under `reason="no_count"`.

**Calls that bill nothing.** A call LiteLLM reports as failed (`status`
`failure`) and a call LiteLLM answered from its own response cache
(`cache_hit` `true`) are skipped: the provider charged nothing for them.
Prompt-cache reads (`cache_read`) are a different thing, and are billed.

### Which LiteLLM calls bill

Velox bills a call by its token counts. It skips only kinds of calls whose
tokens it does not price.

- **Billed:** chat completions (`/v1/chat/completions`), text completions,
  embeddings, `/v1/messages` (Claude Code), `/v1/responses`, Gemini
  `generateContent` and `streamGenerateContent`, and LiteLLM's provider
  pass-through routes such as `/anthropic/v1/messages`. Through `/openai/*`
  pass-through, only chat completions carry tokens.
- **Not billed:** images, audio (speech, transcription, realtime, Vertex AI
  Live), moderation, rerank, search, OCR and video. Also batches, files,
  fine-tuning, assistants, vector stores, containers, evals, MCP tools,
  agent-to-agent (A2A) messages, and fetching or cancelling a stored
  response.

A skipped call that carries tokens is logged as a WARN with its
`call_type`, and counted in
`velox_litellm_skipped_with_tokens_total{call_type}`. Image calls on
`gpt-image-*` models do this every time; a call type that is new to you
there means LiteLLM sent usage that Velox does not bill.

A model you have no price for is not charged: its usage shows as usage
with no price. Gemini models have no ready-made prices, so Gemini usage
shows that way until you price it. But if your `tokens` meter has a default
price, or a price that does not name a `model`, that price charges it.

Known limits:

- LiteLLM 1.83.9's code does not pass a customer on `/vllm/*` and
  `/azure/*` pass-through calls, so Velox refuses them.
- A `/v1/responses` call that uses `file_search` on a provider without it
  makes two model calls, and LiteLLM reports the tokens of the second only.

## 5. Check that it worked

Make one call through the proxy for your customer, then list the
customer's usage:

```bash
CUSTOMER_ID=$(curl -s "$VELOX/v1/customers?external_id=acme-1" \
  -H "Authorization: Bearer $VELOX_KEY" | jq -r '.data[0].id')
curl -s "$VELOX/v1/usage-events?customer_id=$CUSTOMER_ID&limit=5" \
  -H "Authorization: Bearer $VELOX_KEY" | jq -c '.data[] | {quantity, dimensions}'
```

**You see:** one line for each event, newest first, such as:

```text
{"quantity":"200000","dimensions":{"model":"claude-sonnet-4.5","model_raw":"claude-sonnet-4-5-20250929","provider":"anthropic","token_type":"output"}}
{"quantity":"1000000","dimensions":{"model":"claude-sonnet-4.5","model_raw":"claude-sonnet-4-5-20250929","provider":"anthropic","token_type":"input"}}
```

The usage list takes Velox's own customer ID, so the first command looks
it up from the external ID. The events also show on the **Usage** page in
the dashboard.

## Response

`POST /v1/integrations/litellm/spend` answers 200 with counts, plus an
`errors` list when some rows were refused. For a call whose `user` is not a
Velox customer yet, both of its events are held:

```json
{
  "accepted": 0,
  "deduplicated": 0,
  "held": 2,
  "skipped": 0,
  "cache_hits": 0,
  "after_close": 0,
  "no_price": 0,
  "not_on_plan": 0,
  "outside_subscription": 0
}
```

| Field | Meaning |
|---|---|
| `accepted` | New events Velox saved. |
| `deduplicated` | Events Velox already had, or already held, from an earlier send of the same call. |
| `held` | Events Velox kept but did not bill, because the customer or the `tokens` meter does not exist yet. See the next section. |
| `skipped` | Calls that bill nothing: a kind of call Velox does not bill (see [Which LiteLLM calls bill](#which-litellm-calls-bill)), a call with zero tokens, a failed call, or a LiteLLM response-cache hit. |
| `cache_hits` | The LiteLLM response-cache hits among `skipped`. LiteLLM's own spend counts them as $0. |
| `after_close` | Of `accepted`, events whose billing period was already invoiced, such as a replay after the invoice was built. They are saved but billed on no invoice; bill them with a one-off invoice. A resent event counted in `deduplicated` is never counted here. |
| `no_price` | Of `accepted`, events no price of the customer's plan takes: usually a model the plan has no price for. They are saved but billed on no invoice. Add a price for the model; it applies from the next billing period. |
| `not_on_plan` | Of `accepted`, events in a billing period not invoiced yet, of a meter the customer's plan has no price for. Saved, billed on no invoice. |
| `outside_subscription` | Of `accepted`, events whose time no subscription of the customer bills: there is none yet, or the call was before the day it started, in a free trial (also on the day the trial ends, before its end), after it ended or after its scheduled cancel. Saved, billed on no invoice. |
| `errors` | Rows Velox refused and did not keep: a call with no `user` or no `id`, or a record that is not valid. |

An event after the end of the customer's current billing period is not
checked until that period's invoice is built (an hour after the period
end by default), so it is counted in none of `after_close`, `no_price` and
`not_on_plan`. Its own period's invoice bills it if a price takes it.

A refused row does not fail the batch. LiteLLM sees a 200 and treats the
batch as delivered, and does not send it again. So a row in `errors` is
lost unless you fix the call and send it again.

The status code answers a different question: could Velox save the batch
at all?

| Status | Meaning | What to do |
|---|---|---|
| 400 | The body is not valid. | Fix the request. |
| `503 authentication_unavailable` | Velox could not check the API key. The batch was not saved. | Send the whole batch again. |
| `503 ingest_unavailable` | Velox could not reach or write its usage store. The batch was not saved. | Send the whole batch again. |

When you send a batch again, rows that were saved before the failure come
back as `deduplicated`. With `max_retries` set as in step 2, LiteLLM does
this for you.

## Usage for a customer Velox does not know yet

When a call's `user` matches no customer, or the `tokens` meter does not
exist, Velox holds the call's events instead of billing them:

- **Kept.** Each event is stored with its idempotency key, quantity,
  labels and time. Nothing is lost.
- **Shown.** The **Usage** page shows a notice such as "2 usage rows held:
  customer 'acme-2' does not exist yet", with what to do.
  `GET /v1/usage-events/held` returns the same list.
- **Released once.** Create the customer (or the meter), then click
  **Release** on the notice, or call:

  ```bash
  curl -s -X POST "$VELOX/v1/usage-events/held/release" \
    -H "Authorization: Bearer $VELOX_KEY" \
    -H "Content-Type: application/json" \
    -d '{"external_customer_id":"acme-2"}'
  ```

  Each held event becomes a usage event under its original idempotency
  key and time, so a call LiteLLM sent again after you created the
  customer is not counted twice. Releasing twice adds nothing. If the
  `user` value had a typo, pass `"customer_id":"vlx_cus_…"` to release
  into the right customer. One release takes at most 5,000 events;
  `more_to_release` counts the rest, and releasing again takes them.
- **Resent after the customer exists.** If LiteLLM sends a held call again
  after you create the customer, Velox records it then and marks the held
  events released. There is nothing left to release for that call.
- **Discarded, if wrong.** **Discard** (or `POST
  /v1/usage-events/held/discard`) marks the events as never to be billed.
  Velox keeps them as a record. If LiteLLM sends a discarded call again,
  even after you create the customer, it is counted as `deduplicated` and
  not billed.

**What a release bills.** A released event counts toward the billing
period its time falls in, the same rule as any late usage (see
[ADR-120](../adr/120-invoice-grace-period-for-late-usage.md)):

- In a billing period not invoiced yet, and a price of the plan that
  bills that time takes it: that period's invoice includes it.
- In a billing period not invoiced yet, and that plan prices the meter but
  no price takes the event (a model with no price): no invoice charges it.
- In a billing period not invoiced yet, on a meter the plan does not
  price: no invoice charges it. Add the meter to the plan if later usage
  should be charged.
- After the end of the subscription's current period, before that
  period's invoice is built: counted as `in_open_period` without a price
  check. Its own period's invoice includes it if a price takes it.
- Before the day the subscription started, or in a period whose invoice is
  already built: stored, but no subscription invoice includes it. For a customer
  created after the calls, this is the usual case. Bill it with a one-off
  invoice if it should be charged.
- Inside the current period, but before the end of a spend-threshold
  invoice of that period: that invoice already covered the time. If it is
  paid or written off, no invoice includes the event, as above. If it is
  not paid yet, the period's invoice includes it only if the
  spend-threshold invoice is voided first.

The release answer counts each kind (`in_open_period`, `no_price`,
`not_on_plan`, `outside_open_period`, and `pending_threshold_invoice` for the unpaid
spend-threshold case; the paid case counts as `outside_open_period`). Events whose meter still does not exist, or whose
time is after the customer's current time (a customer on a test clock set
in the past), stay held, counted in `still_held`.

The Usage page lists the 200 customers whose usage was held most recently.
When more are held, the notice says how many it does not list. `GET
/v1/usage-events/held` returns `total_customers` and `total_rows` for all
of them.

## Things to know

- **LiteLLM's cost figure does not set the bill.**
  - What the customer pays comes from your prices in Velox.
  - What the usage cost you comes from your
    [provider costs](../ai-pricing.md#see-your-margin) (`PUT
    /v1/provider-costs`). Velox saves that cost on each event when it
    arrives.
  - LiteLLM's `response_cost` is one total for the whole call, which can
    span three events. So Velox never saves it on an event.
  - When the record has `cost_breakdown`, each event gets LiteLLM's cost
    for its own tokens instead of your provider cost: `output_cost` on
    `output`, `cache_read_cost` on `cache_read`, and `input_cost` minus
    `cache_read_cost` on `input` (LiteLLM's `input_cost` includes cache
    reads and cache writes). If there were cache reads but no
    `cache_read_cost`, `input` and `cache_read` use your provider costs.
- **A Bedrock application inference-profile ARN, a `bedrock/<region>/`
  path, or an aggregator's own name** (such as
  `openrouter/anthropic/claude-sonnet-4.6`) does not match a model. Its
  usage keeps the raw name and has no price until you add one.
- **Claude on AWS Bedrock or Google Vertex has a different provider.**
  The `provider` label comes from LiteLLM's `custom_llm_provider`. Claude
  through Bedrock arrives as `bedrock`, and through Vertex as `vertex_ai`,
  not `anthropic`. Provider costs match on provider, so add costs for
  `bedrock` or `vertex_ai` too. Without them, those events have no cost,
  and the margin report counts them as unresolved.
- **The `tokens` meter must exist.** Without it, each event is held, the
  same as for an unknown `user`, until you create the meter and release
  it.
- **One Velox account for each API key.** Each key belongs to one tenant
  (one separate billing account in Velox). A LiteLLM proxy that serves
  several tenants must send each tenant's usage with that tenant's key. A
  field in the call's metadata cannot choose the tenant.

**Next:** [see your margin per customer](../ai-pricing.md#see-your-margin).
