# Velox

### Meter every token. Sell prepaid commits. Know your margin.

**The open-source billing engine for AI and usage-heavy SaaS — runs in your own VPC.**

Velox owns the billing layer above the card charge: pricing, subscriptions, usage metering, invoicing, credits, and dunning (the automatic retry-and-escalate process that runs when a payment fails). Stripe still executes the card charge underneath, as a plain PaymentIntent. So Stripe Billing's percentage fee does not apply, and your usage and invoice records stay in your own Postgres.

[![CI](https://github.com/getvelox/velox/actions/workflows/ci.yml/badge.svg)](https://github.com/getvelox/velox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-green)](LICENSE)
[![Release](https://img.shields.io/github/v/tag/getvelox/velox?label=release)](CHANGELOG.md)

**Pre-1.0.** The public API is stabilising but not yet frozen. Until 1.0.0, breaking changes land on MINOR releases ([versioning policy](CHANGELOG.md)).

---

## What it looks like

One meter with model and token type on each event, and the invoice it produced a month later. [`scripts/demo.sh`](scripts/demo.sh) generates it against a running Velox:

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

Prereqs: Docker, Go 1.26+, Node 22+ (dashboard), `jq` (demo script).

```bash
git clone https://github.com/getvelox/velox.git && cd velox

# Backend: Postgres, a demo tenant, an operator user, API keys
cp .env.example .env
docker compose up -d postgres
make bootstrap       # prints the operator login and the API keys
make dev             # API on :8080

# Operator dashboard (separate terminal)
cd web-v2 && npm install && npm run dev
# → http://localhost:5173, sign in with the login from bootstrap
```

Then run the whole flow, from token usage to a finalized invoice with a PDF and a margin report:

```bash
./scripts/demo.sh <vlx_secret_test_... from make bootstrap>
```

The script checks every call and stops at the first mismatch. Each run creates a fresh demo customer on its own test clock, so you can rerun it.

To run it for real, on one VM with Docker Compose: [`docs/self-host.md`](docs/self-host.md).

---

## Why use it

- **Price AI usage the way it is sold.** Model × token type on one meter, prepaid commits that usage draws down, and a base fee plus usage on one invoice. [AI pricing in code](docs/ai-pricing.md)
- **If you are building usage billing around Stripe yourself.** Velox is that layer, already built: usage records that a retried request does not count twice, prepaid credit that usage draws down, a different price for one customer, invoices, and retries when a payment fails. Stripe still takes the payment. [Why Velox exists](docs/why-velox.md)
- **Know your margin.** Provider cost is stamped on every event, so "which customers lose us money?" is one API call. [What's in the box](docs/features.md)
- **Run it yourself, simply.** One Go binary, Postgres, and Redis for rate limiting. No queue, no warehouse, no workflow engine. [Self-host](docs/self-host.md) · [Will it take our volume?](docs/scale.md)
- **MIT, and nothing is gated.** No licence key, no paid tier, no telemetry. Dunning, credit notes, refunds and the audit log are all in the one edition. [What's in the box](docs/features.md#self-host-first)
- **Test a billing change before it bills anyone.** Test clocks move one test customer through months of billing in seconds. [What's in the box](docs/features.md#stripe-grade-primitives-already-shipped)
- **Built so the invoice is right.** A billing period cannot be invoiced twice, because the database refuses it. [How Velox is engineered](docs/ENGINEERING.md)

---

## What Velox is **not**

Velox is the wrong choice if you need:

- **Simple per-seat pricing paid by card.** Stripe Billing already does that well.
- **A payment processor other than Stripe.** Stripe is the only one today.
- **A marketplace that splits payouts** between sellers (Stripe Connect). Velox bills your own customers.
- **Revenue recognition or accounting reports.** Export the data to your own warehouse.
- **Quotes or multi-phase contracts** for sales-led deals.
- **Many payment methods.** Today it is cards through Stripe, or an invoice the customer pays outside Velox.
- **Something that stops a customer at zero credit.** Velox records usage and never refuses it. It gives you a live balance to check, and your own code does the stopping. [Stop usage when credit runs out](docs/stop-usage-when-credit-runs-out.md)

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
