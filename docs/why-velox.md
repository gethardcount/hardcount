# Why Velox exists

Velox is built around three market needs that Stripe Billing structurally cannot serve, plus one that every billing system is judged on.

**1. AI apps price in dimensions, not units.**

- Real model pricing today is `model × token_type × tier`. `token_type` alone has five disjoint roles (`input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h`).
- Stripe's Meter API forces one meter per dimension *combination*. Modelling a single LLM's pricing then takes a wall of meters and ugly subscription wiring.
- Velox puts dimensions on the event and lets one meter carry them all.
- If you already run a [LiteLLM](integrations/litellm.md) proxy, its spend callback ingests straight into Velox, with no SDK.

**2. AI infra sells commit + usage.**

- "Pay $9k up front, get $10k of usage to draw down" is the default AI-infra contract (a prepaid commit).
- Stripe Billing has no commit primitive. The engines that do (Orb, Metronome) are closed-source SaaS.
- In Velox a commit is one line on an invoice. When the invoice finalizes, the prepaid balance is funded in one atomic step. Usage drains it, promotional credits first.
- `credit.balance_low / _depleted / _recovered` webhooks drive your top-up nudges.

**3. Regulated businesses can't ship billing data to Stripe's servers.**

- Examples: GDPR-strict EU, India's RBI data-localization rules, healthcare-adjacent SaaS, government procurement.
- Stripe's whole model is "send us the data."
- Velox runs in your VPC. One deployment cleanly serves many internal tenants behind Postgres Row-Level Security.
- The binary makes no outbound calls of its own: no licence check, no usage telemetry, no vendor endpoint. You can confirm this by searching the source.

**4. Every bill gets disputed, and the only answer is the raw events.**

Ask engineers who have run metered billing what vendors get wrong. This is what comes back:

> *"you will get a query on a bill by a customer… and you need to be able to dig into the raw data… to validate there was no billing error."*

Velox is built for that moment:

- Raw events are stored, never pre-aggregated away.
- The rate is **snapshotted onto the invoice line**, so re-pricing tomorrow can't silently rewrite what you billed last month.
- Every usage line links straight to the events behind it, filtered to that customer, meter and period.
- The audit log is append-only, enforced by database triggers rather than convention.

## How it compares

A fact-by-fact comparison with Lago, OpenMeter, Flexprice and Kill Bill, with sources: [How Velox compares](comparison.md).
