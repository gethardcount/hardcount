# Quick start: run Velox and its demo

This page starts Velox on your own computer and runs its demo. The demo bills
one test customer for a month of AI token usage, so you can see the whole
flow: usage in, invoice out. It takes about five minutes, and all you need is
Docker.

Think of the quick start as a demo car at a showroom: you drive it, you don't
build it. To run Velox for real customers, see
[Next steps](#next-steps).

**Who it is for:** anyone deciding whether Velox fits their product.

## Words on this page

| Word | Meaning |
|---|---|
| Test mode | The side of Velox where nothing charges real money. The dashboard opens in it. |
| Meter | A counter for one kind of usage, here AI tokens. |
| Recipe | A ready-made pricing setup that Velox creates in one call. |
| Usage record | One "this customer used this much" line that your app sends to Velox (a usage event). |
| Test clock | A pretend clock for test customers. Moving it forward makes a month pass in seconds. |
| Provider cost | What your AI provider, such as Anthropic, charges you per token. |
| Margin | What you billed a customer, minus what their usage cost you, as a percent of what you billed. |

## Before you start

You need:

- git.
- Docker 24 or newer, with the Compose plugin (`docker compose`).
- A bash shell. macOS and Linux have one. On Windows, use WSL.

You do not need Go, Node, jq or a database. Docker downloads everything else.

## Part 1: Start Velox

1. Download Velox and start it.

   ```bash
   git clone https://github.com/getvelox/velox.git
   cd velox/deploy/compose
   ./quickstart.sh
   ```

   The script makes a private `.env` file with new secrets. Then it starts
   five containers: the Velox server, its dashboard, Postgres, Redis and
   nginx (a web server that puts the server and the dashboard on one
   address). The first start
   downloads the images, which can take a few minutes.

   **You see:**

   ```text
   Velox is running.

     Dashboard   http://localhost:8080
     Email       admin@velox.local
     Password    <your password>
     Test key    vlx_secret_test_…

   Open the dashboard address in your browser and sign in with this email and
   password. The password is shown only once. The dashboard opens in test mode,
   where no real money moves.
   ```

   The script also saves these in `deploy/compose/quickstart-credentials.json`.
   Keep that file private: it also holds a live key, which can charge real
   cards once Stripe is connected.

2. Open `http://localhost:8080` in your browser and sign in with that email
   and password.

   **You see:** Home, with a strip at the top: "Test mode. You're viewing test
   data. No real money moves." Its four cards show zero, because Velox has
   no data yet: MRR (monthly recurring revenue), Revenue, Customers and
   Failed payments.

## Part 2: Run the demo

1. In the same folder (`deploy/compose`), run:

   ```bash
   ./quickstart.sh demo
   ```

   The script reads the test key from `quickstart-credentials.json`. It
   runs the demo in a small throwaway container, so your computer needs no
   curl or jq. The first run downloads that container, which adds a few
   seconds.

   **You see:** nine numbered steps, each ending in a green ✓ line. The
   whole run takes about two minutes.

The demo checks every step. If one fails, it stops there with a ✗ line that
says what went wrong. So a finished run means each step below worked. The IDs (the `vlx_…` values), dates and PDF size differ on every run.

### What each demo step does

1. **Check that Velox is running.**

   **You see:** `✓ Velox is up at http://localhost:8080`

2. **Load ready-made prices for Claude tokens.** One call applies the
   `anthropic_style` recipe. It creates a meter named Tokens, a price for
   each Claude model and each kind of token, and a monthly plan named
   "AI API" with no base fee. It also creates a schedule for retrying failed
   card payments, which takes over from the account's starting "Default
   retries" schedule. The kinds of token are input, output, cache read and cache
   write. The prices start at Anthropic's public list prices, which you can
   change later. If you run the demo again, this step reuses the same setup.

   **You see:** `✓ Prices loaded: a tokens meter, a price for each model and token type, and plan vlx_pln_…`

3. **Create a test clock and a demo customer on it.** The clock starts at
   today's date. The customer is "ACME Corp", and each run makes a new one.

   **You see:** `✓ Customer ACME Corp (vlx_cus_…) is on test clock vlx_tclk_…`

4. **Put ACME Corp on the plan.** This is a subscription: it says which plan
   a customer pays for, and when each month starts. Here the month starts
   today.

   **You see:** `✓ Subscription vlx_sub_…, status active`

5. **Save what the AI provider charges you per token.** These are made-up
   costs for Claude Sonnet 4.5: $1.20 per million input tokens, $4.50 per
   million output tokens and $0.05 per million cache-read tokens. From now
   on, Velox records your cost on each usage record that arrives. Usage that
   arrived earlier stays without a cost.

   **You see:** `✓ Saved 3 costs. Velox now records your cost on each new usage record.`

6. **Send token usage, the way a LiteLLM proxy sends it.** LiteLLM is a
   proxy that sits between your app and AI providers. The demo sends five AI
   calls to the same Velox address that LiteLLM reports to. Each call used
   120,000 prompt tokens (40,000 of them read from the cache) and 35,000
   output tokens. Velox splits each call into three usage records: 80,000
   input, 40,000 cache read and 35,000 output.

   **You see:** `✓ 5 AI calls became 15 usage records: input, cache_read and output for each call`

7. **Move the test clock forward one month.** The demo moves the clock 32
   days ahead, past the end of ACME Corp's first month. While the clock
   moves, Velox closes that month and makes the invoice. The demo waits for
   the clock to finish, for up to 30 seconds.

   **You see:** `✓ The clock now reads <a date 32 days from now>. Velox billed ACME Corp's month while the clock moved.`

8. **Show the invoice ACME Corp receives.** The demo checks that the
   invoice is finalized (issued and no longer a draft), has a line for each
   model and token type, and that its PDF renders. Nothing is charged: no
   Stripe account is connected in the quick start.

   **You see:**

   ```text
     VLX-000001  total $3.88 USD  status finalized
       Tokens (claude-sonnet-4.5 · input)  → $1.20
       Tokens (claude-sonnet-4.5 · output)  → $2.62
       Tokens (claude-sonnet-4.5 · cache_read)  → $0.06
   ✓ One invoice line per model and token type, and the PDF renders (34417 bytes)
   ```

   The lines are 400,000 input tokens at $3.00 per million, 175,000 output
   tokens at $15.00 per million, and 200,000 cache-read tokens at $0.30 per
   million. Half a cent rounds to the nearest even cent, so $2.625 is $2.62.
   The invoice number goes up by one on each run.

9. **Show your margin on ACME Corp.** Only you see this, never the customer.
   It is what you billed ($3.88) against what the usage cost you ($1.28).

   **You see:**

   ```text
     billed  $3.88
     cost    $1.28
     margin  67.1%
       claude-sonnet-4.5: cost $1.28  billed $3.88
   ✓ Checked: the cost matches your rates and usage, and the billed amount matches the invoice
   ```

At the end, the demo prints two links:

```text
  Open these links to see it in the dashboard (sign in first; it opens
  in test mode, where the demo ran):
    Invoice:  http://localhost:8080/invoices/vlx_inv_…
    Customer: http://localhost:8080/customers/vlx_cus_…
```

## Part 3: See it in the dashboard

1. Open the **Invoice** link.

   **You see:** invoice VLX-000001 for ACME Corp, with the three token lines
   and the $3.88 total. To see the PDF, open the **⋯** menu at the top right and choose
   **Preview PDF** or **Download PDF**.

2. Open the **Customer** link.

   **You see:** ACME Corp, with "Simulated time: <date>. Dates on this page
   follow the test clock." at the top. Below are the subscription, the
   recent invoices and a **Margin** card. The Margin card starts on the
   current period, which is the new, empty month. Choose **Last invoiced
   period** in it to see the demo month.

3. Look around in the left menu.

   **You see:**
   - **Usage**: the 15 usage records.
   - **Pricing**: the Tokens meter and the AI API plan.
   - **Provider costs**: the three costs from step 5.
   - **Test clocks**: the clock named "demo".

### Why Home still reads $0

Home leaves out every customer on a test clock, and everything they own.
Its four cards (MRR, Revenue, Customers and Failed payments) add things up
by real dates, and a test clock's dates are pretend ones. Adding them would
put money on the wrong days. So Home reads $0 after the demo, and that is
expected. Use the two links instead.

## Run it again, stop it, or remove it

| To | Run | You see |
|---|---|---|
| Run the demo again | `./quickstart.sh demo` | The same nine steps, for a new customer and a new invoice number. |
| Stop Velox and keep its data | `./quickstart.sh stop` | "Stopped. Your data is kept. Run ./quickstart.sh to start again." |
| Start it again | `./quickstart.sh` | The same "Velox is running." box. |
| Stop Velox and delete its data (cannot be undone) | `./quickstart.sh destroy` | "Stopped, and the data of this quickstart install is deleted." |

## If something goes wrong

| What you see | What to do |
|---|---|
| A message that a port is already in use | Run `./quickstart.sh destroy`, then `VELOX_PORT=9000 ./quickstart.sh`. The dashboard is then at `http://localhost:9000`. |
| "Velox did not become healthy in three minutes." | Read the log lines it prints above that message. |
| "The first login was created earlier, and its password is shown only once." | The file that kept the password, `quickstart-credentials.json`, was removed. If you still have the password, sign in with it. If not, run `./quickstart.sh destroy` and start again. |
| "Velox is not running. Run ./quickstart.sh first." | Run `./quickstart.sh`, then the demo again. |
| A ✗ line during the demo | It says which check failed, under the step that failed. [Open an issue](https://github.com/getvelox/velox/issues) with that output. |

The quick start runs the latest published release, so changes merged after
that release are not in it yet.

## Next steps

- **Do the same steps yourself, from your own code:**
  [Send usage and get your first invoice](first-invoice.md).
- **Price your own AI usage:** [AI pricing reference](ai-pricing.md).
- **Send usage from a LiteLLM proxy:** [LiteLLM integration](integrations/litellm.md).
- **Run Velox for real customers:** [Docker Compose on one server](../deploy/compose/README.md),
  then [Self-host](self-host.md).
- **Change Velox itself:** [Development setup](../CONTRIBUTING.md#development-setup).
  The demo script is [`scripts/demo.sh`](../scripts/demo.sh).
