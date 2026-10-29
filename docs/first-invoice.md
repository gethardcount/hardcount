# Send usage and get your first invoice

This page bills one test customer for AI tokens, using the API from your
terminal. At the end, you open the invoice in the dashboard.

## Words on this page

| Word | Meaning |
|---|---|
| Plan | What you sell: a currency, a billing period such as monthly, and a price for each kind of usage. |
| Customer | A person or company you bill. Its external ID is your own ID for it, such as the user ID in your database. |
| Subscription | Puts a customer on a plan, and says when each billing month starts. |
| Usage event | One "this customer used this much" record that your app sends. |
| Idempotency key | A unique ID for one usage event. If you send the same event again with the same key, Hardcount counts it once. |

Recipe, meter and test clock: [quick start](quick-start.md#words-on-this-page).

## Before you start

You need:

- Hardcount running on your computer, from the [quick start](quick-start.md).
  You do not need to run its demo.
- `curl` and `jq` (a tool that reads JSON). On macOS: `brew install jq`.
  On Debian or Ubuntu: `sudo apt install jq`.

Read this page at the version you run. The quick start runs v0.9.0, and
[this page as of v0.9.0](https://github.com/gethardcount/hardcount/blob/v0.9.0/docs/first-invoice.md)
matches it. The copy on `main` can be ahead of that release.

Open a terminal in the `hardcount` folder that the quick start downloaded. Save
the address and your test API key in two variables. Every command below
uses them.

<!-- docs-run: command -->
```bash
export HARDCOUNT=http://localhost:8080
HARDCOUNT_KEY=$(jq -r .secret_key_test deploy/compose/quickstart-credentials.json)
export HARDCOUNT_KEY
curl -s "$HARDCOUNT/health"
```

**You see:**

<!-- docs-run: output -->
```json
{"status":"ok"}
```

The key starts with `hc_secret_test_`, so everything on this page happens
in test mode, where no real money moves. Running Hardcount somewhere else? Set
`HARDCOUNT` to its address and `HARDCOUNT_KEY` to a test secret key from **API
keys** in the dashboard.

Some steps send IDs that an earlier step saved, such as the customer ID.
Those steps build the request body with `jq -n`, which puts a shell
variable into the JSON for you.

## Steps

1. **Load ready-made prices for Claude tokens.** One call applies the
   `anthropic_style` recipe. It creates a meter named Tokens, which counts
   usage events named `tokens`, and a monthly plan named "AI API" with no
   base fee. The plan has a price for each Claude model and each kind of
   token. The prices start at Anthropic's public list
   prices. If the recipe is already there (for example, from the demo),
   the call returns the same setup again.

   <!-- docs-run: command -->
   ```bash
   PLAN_ID=$(curl -s -X POST "$HARDCOUNT/v1/recipes/anthropic_style/instantiate" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" \
     -H "Content-Type: application/json" \
     -d '{}' | jq -r '.created_objects.plan_ids[0]')
   echo "$PLAN_ID"
   ```

   **You see:** the plan ID.

   <!-- docs-run: output -->
   ```text
   hc_pln_…
   ```

2. **Create a test clock.** It starts at the current time.

   <!-- docs-run: command -->
   ```bash
   CLOCK_ID=$(jq -n --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
       '{name: "first invoice", frozen_time: $now}' \
     | curl -s -X POST "$HARDCOUNT/v1/test-clocks" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .id)
   echo "$CLOCK_ID"
   ```

   **You see:** the clock ID.

   <!-- docs-run: output -->
   ```text
   hc_tclk_…
   ```

3. **Create a customer on the test clock.** `CUSTOMER_REF` is your own ID
   for the customer. Your app sends it with each usage event, so Hardcount
   knows whose usage it is.

   <!-- docs-run: command -->
   ```bash
   CUSTOMER_REF=acme-1
   CUSTOMER_ID=$(jq -n --arg ref "$CUSTOMER_REF" --arg clock "$CLOCK_ID" \
       '{external_id: $ref, display_name: "ACME Corp",
         email: "billing@acme.com", test_clock_id: $clock}' \
     | curl -s -X POST "$HARDCOUNT/v1/customers" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .id)
   echo "$CUSTOMER_ID"
   ```

   **You see:** the customer ID.

   <!-- docs-run: output -->
   ```text
   hc_cus_…
   ```

   ACME Corp is now on the **Customers** page.

4. **Put the customer on the plan.** This makes a subscription. It starts
   when you create it: its first month starts at 00:00 on the clock's day,
   in your account's billing time zone.
   (To set one up that bills nothing yet, send `"status": "draft"` and
   activate it later.)

   <!-- docs-run: command -->
   ```bash
   jq -n --arg ref "$CUSTOMER_REF" --arg cus "$CUSTOMER_ID" --arg plan "$PLAN_ID" \
       '{code: ($ref + "-ai-api"), display_name: "ACME Corp AI API",
         customer_id: $cus, items: [{plan_id: $plan, quantity: 1}],
         billing_time: "anniversary"}' \
     | curl -s -X POST "$HARDCOUNT/v1/subscriptions" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq '{id, status}'
   ```

   **You see:**

   <!-- docs-run: output -->
   ```json
   {
     "id": "hc_sub_…",
     "status": "active"
   }
   ```

5. **Send usage.** Your app sends one usage event for each thing it
   counts. Here, one AI call used 1,000,000 input tokens and 200,000 output
   tokens of Claude Sonnet 4.5. That makes two events, one for each kind of
   token. Do **5a** to send them yourself, or **5b** if you use a LiteLLM
   proxy. You need only one of them.

   **5a. Send the events yourself.** `event_name` says which meter counts
   the event: the Tokens meter counts `tokens`. `dimensions` are labels: the
   plan's prices pick events by `model` and `token_type`. Give each event its own idempotency key, and send the same
   key again when you retry that event.

   <!-- docs-run: command name=5a -->
   ```bash
   send_tokens() {   # send_tokens <token type> <quantity>
     jq -n --arg ref "$CUSTOMER_REF" --arg type "$1" --arg qty "$2" \
         '{event_name: "tokens", external_customer_id: $ref, quantity: $qty,
           idempotency_key: ($ref + "-call-1:" + $type),
           dimensions: {provider: "anthropic", model: "claude-sonnet-4.5", token_type: $type}}' \
       | curl -s -X POST "$HARDCOUNT/v1/usage-events" \
         -H "Authorization: Bearer $HARDCOUNT_KEY" \
         -H "Content-Type: application/json" \
         -d @- | jq -c '{id, quantity, token_type: .dimensions.token_type, timestamp}'
   }
   send_tokens input 1000000
   send_tokens output 200000
   ```

   **You see:** one line for each event, such as:

   <!-- docs-run: output -->
   ```text
   {"id":"hc_evt_…","quantity":"1000000","token_type":"input","timestamp":"…"}
   {"id":"hc_evt_…","quantity":"200000","token_type":"output","timestamp":"…"}
   ```

   An event without a `timestamp` gets the customer's current time. For a
   customer on a test clock, that is the clock's time.

   **5b. Or send one call the way a LiteLLM proxy does.** A LiteLLM proxy
   reports each AI call to Hardcount, and Hardcount splits it into events by token
   type. This request sends the same record that the proxy sends.
   LiteLLM's `user` is the customer's external ID. To connect a real
   proxy, see [LiteLLM integration](integrations/litellm.md).

   <!-- docs-run: command name=5b -->
   ```bash
   jq -n --arg ref "$CUSTOMER_REF" \
       '{id: ($ref + "-call-1"), call_type: "completion",
         model: "claude-sonnet-4-5-20250929", custom_llm_provider: "anthropic",
         user: $ref,
         usage: {prompt_tokens: 1000000, completion_tokens: 200000}}' \
     | curl -s -X POST "$HARDCOUNT/v1/integrations/litellm/spend" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @-
   ```

   **You see:** two new events, input and output. Every other count is 0.

   <!-- docs-run: output unless=5a -->
   ```json
   {"accepted":2,"deduplicated":0,"held":0,"skipped":0,"cache_hits":0,"after_close":0,"no_price":0,"not_on_plan":0,"outside_subscription":0}
   ```

   Did 5a first? Then you see:

   <!-- docs-run: output if=5a -->
   ```json
   {"accepted":0,"deduplicated":2,"held":0,"skipped":0,"cache_hits":0,"after_close":0,"no_price":0,"not_on_plan":0,"outside_subscription":0}
   ```

   Both ways use the same idempotency keys, so Hardcount counts the tokens once.

   In the dashboard, the **Usage** page lists both events.

6. **Move the test clock forward one month.** The clock moves 32 days
   ahead, past the end of ACME Corp's first month. While it moves, Hardcount
   closes that month and makes the invoice. The first `date` form is for
   macOS, the second for Linux.

   <!-- docs-run: command -->
   ```bash
   LATER=$(date -u -v+32d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "+32 days" +%Y-%m-%dT%H:%M:%SZ)
   jq -n --arg t "$LATER" '{frozen_time: $t}' \
     | curl -s -X POST "$HARDCOUNT/v1/test-clocks/$CLOCK_ID/advance" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .status
   ```

   **You see:**

   <!-- docs-run: output -->
   ```text
   advancing
   ```

   Run this until it prints `ready`. It takes a few seconds.

   <!-- docs-run: command repeat-until-output -->
   ```bash
   curl -s "$HARDCOUNT/v1/test-clocks/$CLOCK_ID" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" | jq -r .status
   ```

   **You see:**

   <!-- docs-run: output -->
   ```text
   ready
   ```

   Testing a dunning policy that pauses or cancels? Move the clock in two
   steps: see [catch-up](glossary.md#testing-with-simulated-time).

7. **Read the invoice.**

   <!-- docs-run: command -->
   ```bash
   INVOICE_ID=$(curl -s "$HARDCOUNT/v1/invoices?customer_id=$CUSTOMER_ID" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" | jq -r '.data[0].id')
   curl -s "$HARDCOUNT/v1/invoices/$INVOICE_ID" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" \
     | jq '{number: .invoice.invoice_number, status: .invoice.status,
            total_cents: .invoice.total_amount_cents,
            lines: [.line_items[] | {description, amount_cents}]}'
   ```

   **You see:**

   <!-- docs-run: output -->
   ```json
   {
     "number": "HC-…",
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
   longer a draft. Nothing is charged: no Stripe account is connected, and
   ACME Corp has no card.

8. **Open the invoice in the dashboard.** Sign in first.

   <!-- docs-run: command -->
   ```bash
   echo "$HARDCOUNT/invoices/$INVOICE_ID"
   ```

   **You see:** the invoice's address in the dashboard.

   <!-- docs-run: output -->
   ```text
   http://localhost:8080/invoices/hc_inv_…
   ```

   Open it, and you see the invoice for ACME Corp, with the two token lines
   and $6.00 due. The dashboard marks an invoice that is finalized and not
   paid as **Open**. A yellow **No payment method** notice says the invoice
   is charged once the customer adds a card. To see the PDF, open the **⋯**
   menu next to **Audit log** and choose **Preview PDF**.

Home leaves out customers on a test clock, so its totals still read $0.
That is expected: open the customer from **Customers** instead.

## If something goes wrong

| What you see | What to do |
|---|---|
| A step prints `null` | The request failed. Run the same command without the last `jq` part to see the error message. |
| `external_customer_id is required` in step 5a, or `payload.user is required` in step 5b | `CUSTOMER_REF` is not set. A variable lasts only as long as its terminal. Run the page again from the top in one terminal, with `acme-2` in place of `acme-1` in step 3. |
| `customer "acme-1" not found`, or `"held":2` in step 5b | Step 3 did not run. Step 5a refuses usage for a customer Hardcount does not know. Step 5b holds it instead: create the customer, then release it from the notice on the **Usage** page. |
| `customer with external_id "acme-1" already exists` | You ran this page before. In step 3, change `acme-1` to `acme-2`, then run steps 2 to 8 again. |
| `INVOICE_ID` is `null`, or the invoice has no token lines | The usage went to another customer, or was sent after the clock moved. Run steps 2 to 8 again with a new name in step 3. |
| `after_close`, `no_price`, `not_on_plan` or `outside_subscription` is not 0 in step 5b | Hardcount stored the events but will not bill them. `no_price` means no price of the plan takes their `model` and `token_type`. In step 5a, the same reason is in the event's `billing_status`: run the command without the last `jq` part to see it. |

**Next:** price your own models, sell prepaid credit and see your margin:
[AI pricing](ai-pricing.md).
