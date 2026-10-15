# Velox

### Meter every token. Sell prepaid commits. Know your margin.

**The open-source billing engine for AI and usage-heavy SaaS — runs in your own VPC.**

Velox owns the billing layer above the card charge: pricing, subscriptions, usage metering, invoicing, credits, and dunning (the automatic retry-and-escalate process that runs when a payment fails). Stripe still executes the card charge underneath, as a plain PaymentIntent. So Stripe Billing's percentage fee does not apply. Your usage and invoice records live in your own Postgres. Stripe sees each charge, and also the invoice lines if you use Stripe Tax. Customers can also pay another way, such as a bank transfer, and you mark the invoice paid.

[![CI](https://github.com/getvelox/velox/actions/workflows/ci.yml/badge.svg)](https://github.com/getvelox/velox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)
[![Release](https://img.shields.io/github/v/tag/getvelox/velox?label=release)](CHANGELOG.md)

**Pre-1.0.** The public API is stabilising but not yet frozen. Until 1.0.0, breaking changes land on MINOR releases ([versioning policy](CHANGELOG.md)).

---

## What it looks like

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/readme-invoice-dark.png">
  <img alt="A Velox invoice with one line per model and token type, each priced per 1M tokens, for a total of $3.88." src="docs/assets/readme-invoice-light.png" width="100%">
</picture>

One meter, with model and token type on each event, billed as one line per pair.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/readme-margin-dark.png">
  <img alt="The customer's margin card: $3.88 billed, $1.28 provider cost, 67.1% margin." src="docs/assets/readme-margin-light.png" width="100%">
</picture>

What the customer was billed against what the model provider charged you for the same tokens.

`./quickstart.sh demo` makes an invoice like this and prints the same numbers:

```text
VLX-000001  total $3.88 USD  status finalized
  Tokens (claude-sonnet-4.5 · input)  → $1.20
  Tokens (claude-sonnet-4.5 · output)  → $2.62
  Tokens (claude-sonnet-4.5 · cache_read)  → $0.06

billed  $3.88
cost    $1.28
margin  67.1%
```

---

## Quick start

You need git and Docker 24 or newer with the Compose plugin. It pulls the latest published release, so nothing is built. Changes merged after that release are not in it until the next release.

```bash
git clone https://github.com/getvelox/velox.git
cd velox/deploy/compose
./quickstart.sh
```

It prints the dashboard address (`http://localhost:8080`), a login and a test API key. Then see the whole flow, from token usage to a finalized invoice with a PDF and a margin report:

```bash
./quickstart.sh demo
```

The demo checks each step and stops if anything goes wrong, so a finished run means it all worked. Each run creates a fresh demo customer on its own test clock, so you can rerun it. `./quickstart.sh stop` stops Velox and keeps its data; `./quickstart.sh destroy` deletes it.

To change Velox itself, with Go and Node on your machine, see [Development setup](CONTRIBUTING.md#development-setup).

To run it for real, on one VM with Docker Compose: [`docs/self-host.md`](docs/self-host.md).

---

## Why use it

- **Price AI usage the way it is sold.** Model × token type on one meter, prepaid commits that usage draws down, and a base fee plus usage on one invoice. [AI pricing in code](docs/ai-pricing.md)
- **Know your margin.** Set what your model provider charges you, and Velox stamps that cost on each token event that matches a rate. One API call then shows what a customer was billed against what they cost you. [What's in the box](docs/features.md)
- **Run it yourself, simply.** One Go binary, Postgres, and Redis for rate limiting, plus a static dashboard. Background jobs run inside the binary and queue in Postgres, so there is no queue broker, warehouse or workflow engine to run. [Self-host](docs/self-host.md) · [Will it take our volume?](docs/scale.md)
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
