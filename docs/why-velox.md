# Why Velox exists

Velox is built around three needs that Stripe covers only through a hosted product, or not at all, plus one that every billing system is judged on.

**1. AI apps price in dimensions, not units.**

- Real model pricing today is `model × token_type × tier`. `token_type` alone has five disjoint roles (`input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h`).
- Stripe's basic usage-based billing does not price by dimension, so each model and token type needs its own meter. Stripe's [own comparison](https://docs.stripe.com/billing/subscriptions/usage-based/compare-metronome) points to Metronome, its hosted product, for that.
- Velox puts dimensions on the event and lets one meter carry them all.
- If you already run a [LiteLLM](integrations/litellm.md) proxy, its spend callback ingests straight into Velox, with no SDK.

**2. AI infra sells commit + usage.**

- "Pay $9k up front, get $10k of usage to draw down" is a common AI-infra contract (a prepaid commit).
- Stripe's basic usage-based billing has prepaid credit grants but [no commits](https://docs.stripe.com/billing/subscriptions/usage-based/use-cases/credits-based-pricing-model). Orb and Metronome (now part of Stripe) offer commits as hosted services. Open-source engines offer prepaid wallets: see [How Velox compares](comparison.md).
- In Velox a commit is one line on an invoice. When the invoice finalizes, the prepaid balance is funded in one atomic step. Usage drains it, promotional credits first.
- `credit.balance_low / _depleted / _recovered` webhooks drive your top-up nudges.

**3. Some businesses must keep billing records on infrastructure they control.**

- Examples: data-residency policies, healthcare-adjacent SaaS, government procurement.
- Stripe Billing is a hosted service, so usage and invoice data are stored on Stripe's infrastructure.
- Velox runs in your VPC, and usage and invoice records stay in your Postgres. If you connect Stripe for card payments, the customer's name, email, address and the charge amount still go to Stripe.
- Postgres Row-Level Security keeps test data apart from live data, and one tenant's rows apart from another's.
- The binary makes no outbound calls of its own: no licence check, no usage telemetry, no vendor endpoint. You can confirm this by searching the source.

**4. Every bill gets disputed, and the only answer is the raw events.**

One engineer who has built usage billing put it this way ([Hacker News, 2022](https://news.ycombinator.com/item?id=32670188)):

> *"you will get a query on a bill by a customer… and you need to be able to dig into the raw data… and validate there was no billing error."*

Velox is built for that moment:

- Raw events are stored, never pre-aggregated away.
- The rate is **snapshotted onto the invoice line**, so re-pricing tomorrow can't silently rewrite what you billed last month.
- Every usage line links to the usage events for that customer, meter and period.
- The audit log is append-only, enforced by database triggers rather than convention.

## How it compares

A fact-by-fact comparison with Lago, OpenMeter, Flexprice and Kill Bill, with sources: [How Velox compares](comparison.md).
