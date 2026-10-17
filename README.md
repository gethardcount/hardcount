# Velox

### Meter every token. Sell prepaid commits. Know your margin.

**The open-source billing engine for AI and usage-heavy SaaS — runs in your own VPC.**

[![CI](https://github.com/getvelox/velox/actions/workflows/ci.yml/badge.svg)](https://github.com/getvelox/velox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)
[![Release](https://img.shields.io/github/v/tag/getvelox/velox?label=release)](CHANGELOG.md)

If you sell an AI product, Velox works out what each customer owes and bills them. It counts what each customer used, such as tokens, applies your prices, makes the invoice and its PDF, and charges the card through Stripe. You run it on your own servers, so your billing data stays in your own database.

---

## Why Velox

Small AI teams kept running into the same gaps. Velox is built for them.

| The gap | What Velox does |
|---|---|
| **Stripe Billing takes a cut.** Its pay-as-you-go plan charges 0.7% of billing volume, and its basic usage billing can't charge a different rate per model and token type on one meter. | **No Stripe Billing fee.** Velox does the billing and asks Stripe only to charge the card. One meter prices every model and token type. |
| **Open-source engines hold back key parts behind a paid licence**, such as automatic failed-payment retries and invoice previews in Lago. | **One MIT licence, nothing held back.** Retries, credit notes, refunds, previews and the audit log are all included. |
| **They are heavy to run.** Some default setups run nine or eleven services, including Kafka and ClickHouse. | **Three parts:** the Velox server, Postgres and Redis, plus a web dashboard. Try it on your laptop in about five minutes. |
| **No way to fast-forward time** to test a billing change. | **Test clocks.** Move a test customer forward up to a year at a time, so months of billing run in minutes. |
| **No idea which customers lose you money.** AI costs move with usage. | **Margin per customer:** what each request cost you, next to what you billed. |

Stripe still charges its normal card fee, and Stripe Tax's fee if you use it. Every claim above has a source in [Why Velox exists](docs/why-velox.md), and [How it compares](docs/comparison.md) lists where the other projects are ahead.

**Pre-1.0.** Velox is young: one maintainer, Stripe and cards only, no SDKs yet. Until 1.0.0, breaking API changes land on minor releases ([versioning policy](CHANGELOG.md)).

---

## What it looks like

One month of AI usage for one customer, straight from the [demo](#quick-start).

**1. The invoice your customer gets.** One line per model and token type, priced per million tokens.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/readme-invoice-dark.png">
  <img alt="The demo invoice VLX-000001 in the Velox dashboard: input, output and cache_read lines for claude-sonnet-4.5 and input and output lines for claude-haiku-4.5, priced per 1M tokens, $5.73 due, with the activity timeline below." src="docs/assets/readme-invoice-light.png" width="100%">
</picture>

**2. Your margin, per model.** What each model earned you: Sonnet 67.1%, Haiku 20.0%. Velox records what each request cost you, from your provider price table or LiteLLM's reported cost.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/readme-margin-dark.png">
  <img alt="The customer's margin card for the last invoiced period: $5.73 usage billed, $2.76 provider cost, 51.9% margin, and one row per model: claude-sonnet-4.5 billed $3.88 at $1.28 cost, 67.1% margin; claude-haiku-4.5 billed $1.85 at $1.48 cost, 20.0% margin." src="docs/assets/readme-margin-light.png" width="100%">
</picture>

---

## Quick start

Try Velox on your own computer in about five minutes. Think of it as a demo car at a showroom: you drive it, you don't build it.

You need git and Docker 24 or newer with the Compose plugin. On Windows, run it inside WSL.

**1. Start Velox**

```bash
git clone https://github.com/getvelox/velox.git
cd velox/deploy/compose
./quickstart.sh
```

**You see:** "Velox is running.", then your dashboard address, email, password and a test API key. The password is shown only once; the script also saves all four in `deploy/compose/quickstart-credentials.json`.

Open `http://localhost:8080` and sign in. Velox opens in test mode, where no real money moves, and it is empty.

**2. Run the demo**

```bash
./quickstart.sh demo
```

In about two minutes, the demo sets up AI token prices, creates a customer and sends token usage the way a LiteLLM proxy would. Then it moves the customer's test clock (a pretend clock for test customers) to the end of the month, and Velox makes the invoice, its PDF and a margin report.

**You see:** nine steps, each ending in a green ✓, then links to the invoice and the customer. Home still reads $0, because its totals leave out test-clock customers; use the links instead. [Each demo step, and where to click](docs/quick-start.md)

**3. Stop or remove it**

`./quickstart.sh stop` keeps your data. `./quickstart.sh destroy` deletes it, and cannot be undone. If port 8080 is taken, start with `VELOX_PORT=9000 ./quickstart.sh`.

**Next:** [send your own usage and get a first invoice](docs/first-invoice.md) · [run it for real on one VM](docs/production-one-vm.md) · [change Velox itself](CONTRIBUTING.md#development-setup)

---

## Is Velox right for you?

**A good fit if:**

- **You charge by usage,** most of all an AI product that prices per token, by model and token type.
- **You want billing on your own servers.** Usage and invoice records stay in your own Postgres. Stripe sees each charge, and the invoice lines too if you use Stripe Tax.
- **You are a small or mid-size team.** One engineer can run it, on one server or several.
- **Your volume is up to about a million usage events a day.** That is roughly 400,000 AI requests, since each request sends two or three events (input, output, cache). In load tests, a mid-size Postgres (32 GB of memory) took a steady 1,000 events a second. Velox keeps every raw event, so plan about 15 to 20 GB of database disk per month at that volume. [How we measured](docs/scale.md)

**Not a fit if you need:**

- **Simple per-seat pricing paid by card.** Stripe Billing already does that well.
- **Automatic charging through a processor other than Stripe.** Stripe is the only one built in today. [Tell us which one you need](https://github.com/getvelox/velox/issues).
- **A marketplace that splits payouts** between sellers (Stripe Connect). Velox bills your own customers.
- **Revenue recognition or accounting reports.** Export the data to your own warehouse.
- **Quotes or multi-phase contracts** for sales-led deals.

---

## What's inside

- **Pricing:** flat, per-unit, graduated and package prices; rates per model and token type on one meter; a base fee plus usage on one invoice; prepaid commits that later usage draws down. [AI pricing](docs/ai-pricing.md)
- **Billing you can trust:** each period close locks the subscription and checks its period, and Postgres refuses a second period-end invoice for the same subscription and period. [How Velox is engineered](docs/ENGINEERING.md)
- **Getting paid:** cards through Stripe, with automatic retries and emails when a payment fails (dunning). Customers can also pay another way, such as a bank transfer, and you mark the invoice paid.
- **Running it:** one Go program with its background jobs inside, Postgres and Redis. No telemetry and no licence check. [Self-host](docs/self-host.md)

Every feature: [What's in the box](docs/features.md).

---

## Docs

[`docs/README.md`](docs/README.md) lists every doc by what you want to do: decide if Velox fits, try it, connect your product, run it in production, or contribute. Good places to start:

- [How Velox works](docs/how-velox-works.md): from a usage event to a paid invoice, in one picture
- [How it compares](docs/comparison.md) with Lago, OpenMeter, Flexprice and Kill Bill
- [Roadmap](docs/roadmap.md) · [Changelog](CHANGELOG.md)

---

## Community and contributing

- **Questions, or evaluating a deployment:** [GitHub Discussions](https://github.com/getvelox/velox/discussions)
- **Bugs:** [Issues](https://github.com/getvelox/velox/issues). Money-path bugs are triaged first.
- **Security:** [`SECURITY.md`](SECURITY.md). Report privately, not in a public issue.
- **Contributing:** [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

[MIT](LICENSE)
