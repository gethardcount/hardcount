# Send usage and get your first invoice

This page bills one test customer for AI tokens, using the API from your
terminal. At the end, you open the invoice in the dashboard.

## Before you start

You need:

- Hardcount running on your computer, from the [quick start](quick-start.md).
  You do not need to run its demo.
- `curl` and `jq` (a tool that reads JSON). On macOS: `brew install jq`.
  On Debian or Ubuntu: `sudo apt install jq`.

Read this page at the version you run. The quick start runs v0.10.0, and
[this page as of v0.10.0](https://github.com/gethardcount/hardcount/blob/v0.10.0/docs/first-invoice.md)
matches it. The copy on `main` can be ahead of that release.

Open a terminal in the `hardcount` folder that the quick start downloaded
(two folders above `deploy/compose`, where you ran it). Save
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

## Words on this page

| Word | Meaning |
|---|---|
| Recipe | A ready-made pricing setup that Hardcount creates in one call. |
| Meter | Counts the usage events that carry one event name, here `tokens`. A plan's prices read from it. |
| Plan | What you sell: a currency, a billing period such as monthly, and a price for each kind of usage. |
| Customer | A person or company you bill. Its external ID is your own ID for it, such as the user ID in your database. |
| Subscription | Puts a customer on a plan, and says when each billing month starts. |
| Usage event | One "this customer used this much" record that your app sends. |
| Idempotency key | A unique ID for one usage event. If you send the same event again with the same key, Hardcount counts it once. |

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

2. **Create a customer.** `CUSTOMER_REF` is your own ID for the customer.
   Your app sends it with each usage event, so Hardcount knows whose usage
   it is.

   <!-- docs-run: command -->
   ```bash
   CUSTOMER_REF=acme-1
   CUSTOMER_ID=$(jq -n --arg ref "$CUSTOMER_REF" \
       '{external_id: $ref, display_name: "ACME Corp",
         email: "billing@acme.com"}' \
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

3. **Put the customer on the plan.** This makes a subscription. It starts
   when you create it: its first month starts at 00:00 today, in your
   account's billing time zone. `billing_time: "anniversary"` makes each
   month run from the day you create the subscription.

   <!-- docs-run: command -->
   ```bash
   SUBSCRIPTION=$(jq -n --arg ref "$CUSTOMER_REF" --arg cus "$CUSTOMER_ID" --arg plan "$PLAN_ID" \
       '{code: ($ref + "-ai-api"), display_name: "ACME Corp AI API",
         customer_id: $cus, items: [{plan_id: $plan, quantity: 1}],
         billing_time: "anniversary"}' \
     | curl -s -X POST "$HARDCOUNT/v1/subscriptions" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @-)
   SUBSCRIPTION_ID=$(echo "$SUBSCRIPTION" | jq -r .id)
   echo "$SUBSCRIPTION" | jq '{id, status}'
   ```

   **You see:**

   <!-- docs-run: output -->
   ```json
   {
     "id": "hc_sub_…",
     "status": "active"
   }
   ```

4. **Send usage.** Your app sends one usage event for each thing it
   counts. Here, one AI call used 1,000,000 input tokens and 200,000 output
   tokens of Claude Sonnet 4.5. That makes two events, one for each kind of
   token. Do **4a** to send them yourself, or **4b** if you use a LiteLLM
   proxy. You need only one of them.

   Send usage only for a customer Hardcount knows, such as the one from
   step 2. For a customer it does not know, 4a answers with an error, and
   4b holds the events until the customer exists.

   **4a. Send the events yourself.** `event_name` says which meter counts
   the event: the Tokens meter counts `tokens`. `dimensions` are labels: the
   plan's prices pick events by `model` and `token_type`. Give each event its own idempotency key, and send the same
   key again when you retry that event.

   <!-- docs-run: command name=4a -->
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

   An event without a `timestamp` gets the current time.

   **4b. Or send one call the way a LiteLLM proxy does.** A LiteLLM proxy
   reports each AI call to Hardcount, and Hardcount splits it into events by token
   type. This request sends the same record that the proxy sends.
   LiteLLM's `user` is the customer's external ID. To connect a real proxy,
   see [LiteLLM integration](integrations/litellm.md).

   <!-- docs-run: command name=4b -->
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

   In the answer, `accepted` is the count to check: it counts the new
   events.

   **You see:** two new events, input and output. Every other count is 0.

   <!-- docs-run: output unless=4a -->
   ```json
   {"accepted":2,"deduplicated":0,"held":0,"skipped":0,"cache_hits":0,"after_close":0,"no_price":0,"not_on_plan":0,"outside_subscription":0}
   ```

   Did 4a first? Then you see:

   <!-- docs-run: output if=4a -->
   ```json
   {"accepted":0,"deduplicated":2,"held":0,"skipped":0,"cache_hits":0,"after_close":0,"no_price":0,"not_on_plan":0,"outside_subscription":0}
   ```

   Both ways use the same idempotency keys, so Hardcount counts the tokens once.

   In the dashboard, the **Usage** page lists both events.

5. **See your invoice so far.** The preview is ACME Corp's invoice as it
   will be at the end of its month, priced from the usage so far. It
   changes as more usage arrives, and it creates nothing.

   <!-- docs-run: command -->
   ```bash
   jq -n --arg cus "$CUSTOMER_ID" '{customer_id: $cus}' \
     | curl -s -X POST "$HARDCOUNT/v1/invoices/create_preview" \
       -H "Authorization: Bearer $HARDCOUNT_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq '{total_cents: .totals[0].amount_cents,
                    lines: [.lines[] | {description, amount_cents}]}'
   ```

   **You see:**

   <!-- docs-run: output -->
   ```json
   {
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

   In the dashboard, the customer's page shows it under **Upcoming
   invoices**.

6. **Issue the invoice now.** A subscription's invoice is issued when its
   month ends. ACME Corp is a test customer, so you need not wait a month:
   canceling its subscription issues its final invoice now, for the usage
   so far.

   This ends ACME Corp's subscription, and usage sent for it afterwards is
   not billed. To keep trying things, create a new customer: run the page
   again from step 2, with `acme-2` in place of `acme-1`.

   <!-- docs-run: command -->
   ```bash
   curl -s -X POST "$HARDCOUNT/v1/subscriptions/$SUBSCRIPTION_ID/cancel" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" | jq -r .status
   ```

   **You see:**

   <!-- docs-run: output -->
   ```text
   canceled
   ```

   A real customer's invoice is issued by itself when its month ends.
   Canceling a real customer's subscription does what it did here: it ends
   the subscription and issues their final invoice at once.

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
         "description": "Tokens (claude-sonnet-4.5 · input) - canceled mid-period",
         "amount_cents": 300
       },
       {
         "description": "Tokens (claude-sonnet-4.5 · output) - canceled mid-period",
         "amount_cents": 300
       }
     ]
   }
   ```

   The lines are 1,000,000 input tokens at $3.00 per million, and 200,000
   output tokens at $15.00 per million, the same as the preview. Each line
   says "canceled mid-period" because the cancel ended the month early; an
   invoice issued at the end of a month has no such words. "Finalized"
   means issued and no longer a draft. Nothing is charged: no Stripe
   account is connected, and ACME Corp has no card.

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
   paid as **Open**. A yellow **No payment method** notice says Stripe isn't
   connected in this mode, so no card can be added online. Its second line
   says the server can't send email. Without Stripe, Hardcount emails the
   customer the invoice itself, with a link to view it, but the quick start
   sets up no email server, so nothing was sent. To see
   the PDF, open the **⋯** menu next to **Audit log** and choose **Preview
   PDF**.


## If something goes wrong

| What you see | What to do |
|---|---|
| A step prints `null` | The request failed. Run the same command without the last `jq` part to see the error message. |
| `external_customer_id is required` in step 4a, or `payload.user is required` in step 4b | `CUSTOMER_REF` is not set. A variable lasts only as long as its terminal. Run the page again from the top in one terminal, with `acme-2` in place of `acme-1` in step 2. |
| `customer "acme-1" not found`, or `"held":2` in step 4b | Step 2 did not run. Step 4a refuses usage for a customer Hardcount does not know. Step 4b holds it instead: run steps 2 and 3, then release it from the notice on the **Usage** page. |
| `customer with external_id "acme-1" already exists` | You ran this page before. In step 2, change `acme-1` to `acme-2`, then run steps 2 to 8 again. |
| `INVOICE_ID` is `null`, or the invoice has no token lines | The usage went to another customer, or was sent after the cancel in step 6. Run steps 2 to 8 again with a new name in step 2. |
| `after_close`, `no_price`, `not_on_plan` or `outside_subscription` is not 0 in step 4b | Hardcount stored the events but will not bill them. `no_price` means no price of the plan takes their `model` and `token_type`. In step 4a, the same reason is in the event's `billing_status`: run the command without the last `jq` part to see it. |

**Next:**

- Charge this invoice to a test card:
  [Connect Stripe and take card payments](guides/connect-stripe.md).
- Price each AI model and kind of token, and turn a provider's token
  counts into usage events:
  [Bill for AI tokens by model](guides/bill-for-ai-tokens.md).
