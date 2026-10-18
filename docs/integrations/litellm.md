# LiteLLM integration

LiteLLM is an open-source proxy that sits between your app and many AI
providers. This page connects it to Velox, so that every AI call your
customers make becomes usage in Velox. You write no code: you add a few
lines to the proxy's config file.

Think of it as a receipt printer on the proxy. After each AI call, LiteLLM
sends Velox a record of the tokens used, and Velox turns it into usage
events for that customer.

**Who it is for:** teams that already run a LiteLLM proxy. Not using
LiteLLM? Send usage with the API instead: see [Send usage and get your
first invoice](../first-invoice.md).

## Before you start

Set these up before LiteLLM sends its first call. Usage that arrives
before the customer or the meter exists is held, not billed: see
[Usage for a customer Velox does not know yet](#usage-for-a-customer-velox-does-not-know-yet).

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

> **Create the customer first.** If LiteLLM sends usage for a `user` that
> Velox does not know, Velox holds that usage: it keeps it, does not bill
> it, and shows it on the **Usage** page. After you create the customer,
> you release it. Usage from before the customer's subscription started
> is stored but not invoiced, so creating customers first is still the
> way to bill every call.

## 1. Load prices for your models

The easiest way is a recipe: a ready-made pricing setup that Velox creates
in one call. It creates the `tokens` meter (a counter for tokens), a price
for each model and kind of token, and a monthly plan. Use
`anthropic_style` for Claude models or `openai_style` for OpenAI models.
[AI pricing](../ai-pricing.md#ready-made-recipes) lists the models each one
prices.

```bash
curl -s -X POST "$VELOX/v1/recipes/anthropic_style/instantiate" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{}' | jq -r '.created_objects.plan_ids[0]'
```

**You see:** the plan ID, such as `vlx_pln_…`.

The API key picks test or live mode: `vlx_secret_test_…` or
`vlx_secret_live_…`. There is no `livemode` field in the request.

You can also create the meter by hand. You then also need a price for each
model and kind of token
([how](../ai-pricing.md#price-a-model-a-recipe-does-not-cover)):

```bash
curl -s -X POST "$VELOX/v1/meters" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"key":"tokens","name":"Tokens","unit":"tokens","aggregation":"sum"}'
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
  5, 10, 20 and 40 seconds apart cover that. Sending a batch again is safe,
  because Velox counts each record once.

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

A call without `user` is refused with `payload.user is required`. The rest
of the batch is still accepted. The refusal is a row in the `errors` list
of the 200 response (see [Response](#response)).

## 4. What lands in Velox

Each AI call becomes up to three usage events on the `tokens` meter. The
`token_type` label tells them apart, and no token is counted twice.
LiteLLM's `prompt_tokens` already includes tokens read from the cache, so
Velox takes those out of `input`.

| `token_type` | Quantity | Idempotency key |
|---|---|---|
| `input` | `prompt_tokens` minus the cached tokens | `<litellm_id>:input` |
| `cache_read` | `prompt_tokens_details.cached_tokens` or `cache_read_input_tokens`, if any | `<litellm_id>:cache_read` |
| `output` | `usage.completion_tokens` | `<litellm_id>:output` |

The idempotency key is a unique ID for each event. If LiteLLM sends the same
call again, Velox counts it once.

Each event also carries these labels (dimensions):

- `model`: the model family name that prices are set on. Velox turns
  LiteLLM's model name into it, for example `claude-sonnet-4-5-20250929`
  becomes `claude-sonnet-4.5`.
- `model_raw`: the model name exactly as LiteLLM sent it.
- `provider`: the provider, from LiteLLM's `custom_llm_provider`.
- `team_id`, when the call has one in `metadata`.
- `request_tags`, only when the tags arrive under `metadata`. A current
  proxy sends its tags outside `metadata`, so they are not a label today.

Each event also keeps the LiteLLM call ID, LiteLLM's own cost figure (for
reference only), and the call's original metadata under
`litellm_metadata.*`.

**Cache-write tokens** (`cache_creation`) have no `token_type` of their own
yet, because LiteLLM does not say which cache lifetime they used (5 minutes
or 1 hour). LiteLLM 1.83.9 counts them in `prompt_tokens` for Anthropic and
Bedrock, so they are in the `input` event and billed at your input price.
Velox logs a warning when it sees them.

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
  "skipped": 0
}
```

| Field | Meaning |
|---|---|
| `accepted` | New events Velox saved. |
| `deduplicated` | Events Velox already had, or already held, from an earlier send of the same call. |
| `held` | Events Velox kept but did not bill, because the customer or the `tokens` meter does not exist yet. See the next section. |
| `skipped` | Calls with no tokens to bill, such as image generation, moderation, or a failed call with zero tokens. |
| `errors` | Rows Velox refused and did not keep: a call with no `user` or no `id`, or a record that is not valid. |

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

- Inside the customer's current billing period, on a meter the
  subscription's plan prices: the next invoice for that period includes it.
- Inside the current billing period, on a meter the plan does not price:
  no invoice charges it. Add the meter to the plan if it should be charged.
- Before the subscription started, or in a period whose invoice is already
  built: stored, but no subscription invoice includes it. For a customer
  created after the calls, this is the usual case. Bill it with a one-off
  invoice if it should be charged.

The release answer counts each kind (`in_open_period`, `not_on_plan`,
`outside_open_period`). Events whose meter still does not exist, or whose
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

Related: [ADR-044](../adr/044-canonical-ai-token-metering-model.md), the
decision behind one `tokens` meter with a `token_type` label.
