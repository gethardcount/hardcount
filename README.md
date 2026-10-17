# Velox

### Meter every token. Sell prepaid commits. Know your margin.

**The open-source billing engine for AI and usage-heavy SaaS — runs in your own VPC.**

**In plain words:** if you sell an AI product, Velox works out what each customer owes and bills them. It counts what each customer used, such as tokens. It applies your prices, makes the invoice and its PDF, and charges the card through Stripe. You run it on your own servers, so the billing data stays in your own database.

**Who it is for:** teams that charge their customers by usage, most of all AI products that charge per token, and that want to keep their billing data on their own servers.

**Why it exists:**

- **AI prices are hard to bill.** Each model, and each kind of token (input, output, cached), has its own price. Velox prices them all with one meter (a counter for one kind of usage).
- **Customers can pay up front.** You can sell prepaid credit (a prepaid commit), and later usage draws it down.
- **You see your margin.** Velox shows what you billed each customer next to what your AI provider charged you for their usage.

Velox owns the billing layer above the card charge: pricing, subscriptions, usage metering, invoicing, credits, and dunning (the automatic retry-and-escalate process that runs when a payment fails). Stripe still executes the card charge underneath, as a plain PaymentIntent. So Stripe Billing's percentage fee does not apply. Your usage and invoice records live in your own Postgres. Stripe sees each charge, and also the invoice lines if you use Stripe Tax. Customers can also pay another way, such as a bank transfer, and you mark the invoice paid.

[![CI](https://github.com/getvelox/velox/actions/workflows/ci.yml/badge.svg)](https://github.com/getvelox/velox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)
[![Release](https://img.shields.io/github/v/tag/getvelox/velox?label=release)](CHANGELOG.md)

**Pre-1.0.** The public API is stabilising but not yet frozen. Until 1.0.0, breaking changes land on MINOR releases ([versioning policy](CHANGELOG.md)).

---

## What it looks like

One meter with model and token type on each event, and the invoice it produced a month later. The invoice below is what [`scripts/demo.sh`](scripts/demo.sh) produces against a running Velox, laid out for reading. The script prints the same numbers in plain lines.

```text
ACME Corp — VLX-000001                                    $3.88
──────────────────────────────────────────────────────────────
Tokens (claude-sonnet-4.5 · input)       400,000    →   $1.20
Tokens (claude-sonnet-4.5 · output)      175,000    →   $2.62
Tokens (claude-sonnet-4.5 · cache_read)  200,000    →   $0.06
──────────────────────────────────────────────────────────────
Margin (billed $3.88 vs provider cost $1.28)            67.1%
```

![The full Velox invoice page for a paid invoice: the document with one line per (model, token_type) and unit prices quoted per 1M tokens, and an activity timeline running from cycle close to an operator-recorded bank-transfer payment](docs/assets/invoice-token-lines.png)

One meter carried every token type, prices are stored exactly ($3.00 per 1M tokens), and the margin line comes from provider cost stamped on each event.

---

## Quick start

Try Velox on your own computer in about five minutes. Think of it as a demo car at a showroom: you drive it, you don't build it.

You need git and Docker 24 or newer with the Compose plugin. You do not need Go, Node or a database. The script runs in bash, so it works as is on macOS and Linux. On Windows, run it inside WSL.

**1. Start Velox**

```bash
git clone https://github.com/getvelox/velox.git
cd velox/deploy/compose
./quickstart.sh
```

This downloads the latest release and starts it in Docker: the Velox server, its dashboard, Postgres and Redis. The first start takes longest, because Docker downloads the images.

**You see:** "Velox is running.", then four lines:

```text
  Dashboard   http://localhost:8080
  Email       <your login email>
  Password    <your password>
  Test key    vlx_secret_test_…
```

The password is shown only once. The script also saves all four in `deploy/compose/quickstart-credentials.json`. Keep that file private.

Open `http://localhost:8080` and sign in with that email and password.

**You see:** the dashboard, with a "Test mode" strip at the top: "You're viewing test data. No real money moves." Velox is empty at this point: there are no customers or invoices yet.

**2. Run the demo**

```bash
./quickstart.sh demo
```

The demo fills Velox with an example, so you can watch one month of billing in about two minutes:

1. It sets up prices for AI tokens, with one rate for each model and token type.
2. It creates a demo customer with a subscription.
3. It saves what the AI provider charges you per token, so Velox can work out your margin.
4. It sends token usage for the customer, the way a LiteLLM proxy would.
5. It moves the customer's clock to the end of the month. A test clock is a pretend clock for test customers, so you do not wait 30 days.
6. Velox closes the month and makes the invoice, its PDF and a margin report.

The demo checks each step and stops if one fails, so a finished run means it all worked.

**You see:** nine numbered steps, each ending in a green ✓ line. Step 8 prints the invoice total and its lines, and step 9 prints your margin. Then "Done: every step above ran and was checked", and two links: one to the invoice and one to the customer.

Open the links to see the customer, the usage, the invoice and its PDF. You can run the demo again: each run makes a new demo customer. [What each demo step does, and where to click](docs/quick-start.md).

Home still reads $0 after the demo. That is expected: the Home totals leave out customers on a test clock, and the demo customer is on one. Use the two links instead.

**3. Stop or remove it**

- `./quickstart.sh stop` stops Velox and keeps its data. Run `./quickstart.sh` to start it again.
  **You see:** "Stopped. Your data is kept."
- `./quickstart.sh destroy` stops Velox and deletes its data. This cannot be undone.
  **You see:** "Stopped, and the data of this quickstart install is deleted."

If port 8080 is already in use, start it with `VELOX_PORT=9000 ./quickstart.sh`. The quick start runs the latest published release, so changes merged after that release are not in it yet.

To change Velox itself, with Go and Node on your machine, see [Development setup](CONTRIBUTING.md#development-setup).

To run it for real, on one VM with Docker Compose: [`docs/self-host.md`](docs/self-host.md).

---

## Why use it

- **Price AI usage the way it is sold.** Model × token type on one meter, prepaid commits that usage draws down, and a base fee plus usage on one invoice. [AI pricing in code](docs/ai-pricing.md)
- **Know your margin.** Set what your model provider charges you, and Velox stamps that cost on each token event that matches a rate. One API call then shows what a customer was billed against what they cost you. [What's in the box](docs/features.md)
- **Run it yourself, simply.** One Go binary, Postgres, and Redis for rate limiting, plus a static dashboard. Background jobs run inside the binary and queue in Postgres, so there is no queue broker, warehouse or workflow engine to run. [Self-host](docs/self-host.md) · [How much traffic Velox handles](docs/scale.md)
- **MIT, and nothing is gated.** No licence key, no paid tier, no telemetry. Dunning, credit notes, refunds and the audit log are all in the one edition. [What's in the box](docs/features.md#self-host-first)
- **Test a billing change before it bills anyone.** In test mode, a test clock moves a test customer through months of billing without waiting for real time, up to a year per call. [What's in the box](docs/features.md#billing-building-blocks)
- **Built so the invoice is right.** Every period close locks the subscription and checks its period, and Postgres refuses a second period-end invoice for the same subscription and period. A voided invoice does not count. [How Velox is engineered](docs/ENGINEERING.md)

---

## What Velox is **not**

Velox is the wrong choice if you need:

- **Simple per-seat pricing paid by card.** Stripe Billing already does that well.
- **Automatic charging through a processor other than Stripe.** Stripe is the only one built in today. If you need another, [open an issue](https://github.com/getvelox/velox/issues) and tell us which one.
- **A marketplace that splits payouts** between sellers (Stripe Connect). Velox bills your own customers.
- **Revenue recognition or accounting reports.** Export the data to your own warehouse.
- **Quotes or multi-phase contracts** for sales-led deals.

---

## Docs

[`docs/README.md`](docs/README.md) lists every doc by what you want to do: decide if Velox fits, try it, connect your product, run it in production, or contribute.

- [How Velox works](docs/how-velox-works.md): from a usage event to a paid invoice, in one picture
- [How it compares](docs/comparison.md) with Lago, OpenMeter, Flexprice and Kill Bill
- [Why Velox exists](docs/why-velox.md)
- [How Velox is engineered](docs/ENGINEERING.md)
- [Roadmap](docs/roadmap.md) · [Changelog](CHANGELOG.md)

---

## Community and contributing

- **Questions, or evaluating a deployment:** [GitHub Discussions](https://github.com/getvelox/velox/discussions)
- **Bugs:** [Issues](https://github.com/getvelox/velox/issues). Money-path bugs are triaged first.
- **Security:** [`SECURITY.md`](SECURITY.md). Report privately, not in a public issue.
- **Contributing:** [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

[MIT](LICENSE)
