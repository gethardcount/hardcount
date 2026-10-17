# Send usage and get your first invoice

This page bills one test customer for AI tokens, using the API from your
terminal. It takes about ten minutes. At the end, you open the invoice in
the dashboard.

Think of a taxi meter. Your app reports what the customer used, Velox
counts it and multiplies it by the price, and the total goes on the
invoice. Stripe charges the card at the end, once you connect it.

**Who it is for:** developers who want to connect their own product to
Velox. If you only want to see Velox work, the
[quick start](quick-start.md) demo does all of this for you.

## Words on this page

| Word | Meaning |
|---|---|
| Recipe | A ready-made pricing setup that Velox creates in one call. |
| Meter | A counter for one kind of usage, here AI tokens. |
| Plan | What you sell: a currency, a billing period such as monthly, and the meters it charges for. |
| Customer | A person or company you bill. Its external ID is your own ID for it, such as the user ID in your database. |
| Subscription | Puts a customer on a plan, and says when each billing month starts. |
| Usage event | One "this customer used this much" record that your app sends. |
| Idempotency key | A unique ID for one usage event. If you send the same event again with the same key, Velox counts it once. |
| Test clock | A pretend clock for test customers. Moving it forward makes a month pass in seconds. |

## Before you start

You need:

- Velox running on your computer, from the [quick start](quick-start.md).
  You do not need to run its demo.
- `curl` and `jq` (a tool that reads JSON). On macOS: `brew install jq`.
  On Debian or Ubuntu: `sudo apt install jq`.

Open a terminal in the `velox` folder that the quick start downloaded. Save
the address and your test API key in two variables. Every command below
uses them.

```bash
export VELOX=http://localhost:8080
export VELOX_KEY=$(jq -r .secret_key_test deploy/compose/quickstart-credentials.json)
curl -s "$VELOX/health"
```

**You see:** `{"status":"ok"}`

The key starts with `vlx_secret_test_`, so everything on this page happens
in test mode, where no real money moves. Running Velox somewhere else? Set
`VELOX` to its address and `VELOX_KEY` to a test secret key from **API
keys** in the dashboard.

Some steps send IDs that an earlier step saved, such as the customer ID.
Those steps build the request body with `jq -n`, which puts a shell
variable into the JSON for you.

## Steps

1. **Load ready-made prices for Claude tokens.** One call applies the
   `anthropic_style` recipe. It creates a meter named `tokens`, a price for
   each Claude model and each kind of token, and a monthly plan named "AI
   API" with no base fee. The prices start at Anthropic's public list
   prices. If the recipe is already there (for example, from the demo),
   the call returns the same setup again.

   ```bash
   PLAN_ID=$(curl -s -X POST "$VELOX/v1/recipes/anthropic_style/instantiate" \
     -H "Authorization: Bearer $VELOX_KEY" \
     -H "Content-Type: application/json" \
     -d '{}' | jq -r '.created_objects.plan_ids[0]')
   echo "$PLAN_ID"
   ```

   **You see:** the plan ID, such as `vlx_pln_…`.

2. **Create a test clock.** It starts at the current time.

   ```bash
   CLOCK_ID=$(jq -n --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
       '{name: "first invoice", frozen_time: $now}' \
     | curl -s -X POST "$VELOX/v1/test-clocks" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .id)
   echo "$CLOCK_ID"
   ```

   **You see:** the clock ID, such as `vlx_tclk_…`.

3. **Create a customer on the test clock.** `CUSTOMER_REF` is your own ID
   for the customer. Your app sends it with each usage event, so Velox
   knows whose usage it is.

   ```bash
   CUSTOMER_REF=acme-1
   CUSTOMER_ID=$(jq -n --arg ref "$CUSTOMER_REF" --arg clock "$CLOCK_ID" \
       '{external_id: $ref, display_name: "ACME Corp",
         email: "billing@acme.com", test_clock_id: $clock}' \
     | curl -s -X POST "$VELOX/v1/customers" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .id)
   echo "$CUSTOMER_ID"
   ```

   **You see:** the customer ID, such as `vlx_cus_…`. ACME Corp is now on
   the **Customers** page.

4. **Put the customer on the plan.** This makes a subscription. Its first
   month starts at the clock's current time.

   ```bash
   jq -n --arg ref "$CUSTOMER_REF" --arg cus "$CUSTOMER_ID" --arg plan "$PLAN_ID" \
       '{code: ($ref + "-ai-api"), display_name: "ACME Corp AI API",
         customer_id: $cus, items: [{plan_id: $plan, quantity: 1}],
         billing_time: "anniversary", start_now: true}' \
     | curl -s -X POST "$VELOX/v1/subscriptions" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq '{id, status}'
   ```

   **You see:**

   ```json
   {
     "id": "vlx_sub_…",
     "status": "active"
   }
   ```

5. **Send usage.** Your app sends one usage event for each thing it
   counts. Here, one AI call used 1,000,000 input tokens and 200,000 output
   tokens of Claude Sonnet 4.5. That makes two events, one for each kind of
   token. Do **5a** to send them yourself, or **5b** if you use a LiteLLM
   proxy. You need only one of them.

   **5a. Send the events yourself.** `event_name` is the meter's key.
   `dimensions` are labels: the recipe's prices pick events by `model` and
   `token_type`. Give each event its own idempotency key, and send the same
   key again when you retry that event.

   ```bash
   send_tokens() {   # send_tokens <token type> <quantity>
     jq -n --arg ref "$CUSTOMER_REF" --arg type "$1" --arg qty "$2" \
         '{event_name: "tokens", external_customer_id: $ref, quantity: $qty,
           idempotency_key: ($ref + "-call-1:" + $type),
           dimensions: {provider: "anthropic", model: "claude-sonnet-4.5", token_type: $type}}' \
       | curl -s -X POST "$VELOX/v1/usage-events" \
         -H "Authorization: Bearer $VELOX_KEY" \
         -H "Content-Type: application/json" \
         -d @- | jq -c '{id, quantity, token_type: .dimensions.token_type, timestamp}'
   }
   send_tokens input 1000000
   send_tokens output 200000
   ```

   **You see:** one line for each event, such as:

   ```text
   {"id":"vlx_evt_…","quantity":"1000000","token_type":"input","timestamp":"…"}
   {"id":"vlx_evt_…","quantity":"200000","token_type":"output","timestamp":"…"}
   ```

   An event without a `timestamp` gets the customer's current time. For a
   customer on a test clock, that is the clock's time.

   **5b. Or send one call the way a LiteLLM proxy does.** A LiteLLM proxy
   reports each AI call to Velox, and Velox splits it into events by token
   type. This request sends the same record that the proxy sends.
   LiteLLM's `user` is the customer's external ID. To connect a real
   proxy, see [LiteLLM integration](integrations/litellm.md).

   ```bash
   jq -n --arg ref "$CUSTOMER_REF" \
       '{id: ($ref + "-call-1"), call_type: "completion",
         model: "claude-sonnet-4-5-20250929", custom_llm_provider: "anthropic",
         user: $ref,
         usage: {prompt_tokens: 1000000, completion_tokens: 200000}}' \
     | curl -s -X POST "$VELOX/v1/integrations/litellm/spend" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @-
   ```

   **You see:** `{"accepted":2,"deduplicated":0,"held":0,"skipped":0}`. That is two
   new events: input and output.

   Did 5a first? Then you see `{"accepted":0,"deduplicated":2,"held":0,"skipped":0}`.
   Both ways use the same idempotency keys, so Velox counts the tokens once.

   In the dashboard, the **Usage** page lists both events.

6. **Move the test clock forward one month.** The clock moves 32 days
   ahead, past the end of ACME Corp's first month. While it moves, Velox
   closes that month and makes the invoice. The first `date` form is for
   macOS, the second for Linux.

   ```bash
   LATER=$(date -u -v+32d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "+32 days" +%Y-%m-%dT%H:%M:%SZ)
   jq -n --arg t "$LATER" '{frozen_time: $t}' \
     | curl -s -X POST "$VELOX/v1/test-clocks/$CLOCK_ID/advance" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .status
   ```

   **You see:** `advancing`

   Run this until it prints `ready`. It takes a few seconds.

   ```bash
   curl -s "$VELOX/v1/test-clocks/$CLOCK_ID" \
     -H "Authorization: Bearer $VELOX_KEY" | jq -r .status
   ```

   **You see:** `ready`

7. **Read the invoice.**

   ```bash
   INVOICE_ID=$(curl -s "$VELOX/v1/invoices?customer_id=$CUSTOMER_ID" \
     -H "Authorization: Bearer $VELOX_KEY" | jq -r '.data[0].id')
   curl -s "$VELOX/v1/invoices/$INVOICE_ID" \
     -H "Authorization: Bearer $VELOX_KEY" \
     | jq '{number: .invoice.invoice_number, status: .invoice.status,
            total_cents: .invoice.total_amount_cents,
            lines: [.line_items[] | {description, amount_cents}]}'
   ```

   **You see:**

   ```json
   {
     "number": "VLX-…",
     "status": "finalized",
     "total_cents": 600,
     "lines": [
       {
         "description": "Tokens (claude-sonnet-4.5 · input)",
         "amount_cents": 300
       },
       {
         "description": "Tokens (claude-sonnet-4.5 · output)",
         "amount_cents": 300
       }
     ]
   }
   ```

   The lines are 1,000,000 input tokens at $3.00 per million, and 200,000
   output tokens at $15.00 per million. "Finalized" means issued and no
   longer a draft. Nothing is charged, because no Stripe account is
   connected.

8. **Open the invoice in the dashboard.** Sign in first.

   ```bash
   echo "$VELOX/invoices/$INVOICE_ID"
   ```

   **You see:** a link. Open it, and you see the invoice for ACME Corp,
   with the two token lines and the $6.00 total. To see the PDF, open the
   **⋯** menu at the top right and choose **Preview PDF**.

Home leaves out customers on a test clock, so its totals still read $0.
That is expected: open the customer from **Customers** instead.

## If something goes wrong

| What you see | What to do |
|---|---|
| A step prints `null` | The request failed. Run the same command without the last `jq` part to see the error message. |
| `customer "acme-1" not found`, or `"held":2` in step 5b | Step 3 did not run, or `CUSTOMER_REF` is not set in this terminal. Step 5a refuses usage for a customer Velox does not know. Step 5b holds it instead: create the customer, then release it from the notice on the **Usage** page. |
| `customer with external_id "acme-1" already exists` | You ran this page before. In step 3, change `acme-1` to `acme-2`, then run steps 2 to 8 again. |
| `INVOICE_ID` is `null`, or the invoice has no token lines | The usage went to another customer, or was sent after the clock moved. Run steps 2 to 8 again with a new name in step 3. |

## Next steps

- **Price a model the recipe does not cover, sell prepaid credit, or see
  your margin:** [AI pricing reference](ai-pricing.md).
- **Send usage from a LiteLLM proxy:** [LiteLLM integration](integrations/litellm.md).
- **Let your app react when an invoice is paid or a payment fails:**
  [Webhooks](webhooks.md).
- **Every route:** [API surface](api-surface.md).
