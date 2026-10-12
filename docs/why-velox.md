# Why Velox exists, and how it compares

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

## How it fits

|                          | **Velox** | Stripe Billing | Lago        | Orb / Metronome¹  | OpenMeter²        |
|--------------------------|-----------|----------------|-------------|-------------------|-------------------|
| OSS / self-host          | ✅        | ❌             | ✅          | ❌                | ✅                |
| AI-native pricing        | ✅        | ❌             | ⚠️ generic  | ⚠️ closed source  | ⚠️ metering-first |
| Full billing engine      | ✅        | ✅             | ✅          | ✅                | ✅ beta           |
| Stripe-grade primitives  | ✅        | ✅             | ⚠️          | ✅                | ⚠️                |
| Prepaid commits + drawdown | ✅      | ❌             | ⚠️ wallets  | ✅                | ❌                |
| Per-customer margin (COGS) | ✅ in-app | ❌           | ❌          | ❌ warehouse join | ⚠️ cost, no margin |
| Pricing                  | OSS       | 0.5% of GMV    | OSS / cloud | sales-gated       | OSS / cloud       |
| Licence                  | MIT       | proprietary    | AGPL-3.0    | proprietary       | Apache-2.0        |
| Dunning without paying³  | ✅        | ✅             | ❌          | ✅                | ⚠️ Stripe Invoicing |
| Data sovereignty         | ✅        | ❌             | ⚠️          | ❌                | ✅                |

¹ Metronome was acquired by Stripe (Jan 2026). It is still SaaS-only, so your billing data lives on Stripe's servers either way.

² OpenMeter ([acquired by Kong](https://konghq.com/blog/news/kong-acquires-openmeter), Sep 2025) now runs a full invoice lifecycle; billing is marked beta. It ships LLM cost tables with a per-feature, per-customer cost query. That is cost, not margin: nothing subtracts it from revenue. The difference is shape:

- It runs on Kafka + ClickHouse + Postgres.
- It has no dunning of its own. Collection is handed to Stripe Invoicing (Stripe's percentage fee applies), or to a custom-invoicing integration you build.

Everything in this footnote was checked against the OpenMeter source on 2026-09-28.

³ Open-core self-hosting isn't automatically free of gates. Lago's self-hosted edition checks a `LAGO_LICENSE` key against their licence server. A 30-entry `PREMIUM_INTEGRATIONS` list decides what's enabled. `auto_dunning` is on it, alongside SSO, RBAC, progressive billing and every accounting/CRM integration ([source](https://github.com/getlago/lago-api/blob/main/app/models/organization.rb)). Velox has no licence key and gates nothing. One caveat: some of what Lago gates (SSO, RBAC, revenue recognition) Velox simply doesn't have. See [What Velox is not](../README.md#what-velox-is-not).

**Verified as of 2026-08-17.** On the pricing row, neither Orb nor Metronome publishes an annual list price:

- Orb's three tiers all read "Custom pricing" behind Contact Sales ([pricing](https://www.withorb.com/pricing)).
- Metronome publishes a Starter rate: 0.8% of billing volume plus $0.04 per 1k ingest events. Its Custom tier is behind sales ([pricing](https://metronome.com/pricing)).

Competitor pricing, licensing and ownership all move. Re-check any cell you plan to rely on.

Velox lives in the empty cell: **OSS + self-host + AI-native + full billing engine.**

The decision tree:

- Pick **Stripe Billing** (or Stripe + Metronome) for hosted SaaS billing.
- Pick **Lago** for generic OSS billing without AI-shaped pricing.
- Pick **Orb/Metronome** if you can't self-host and can budget for usage-based contracts.
- Pick **Velox** when you need AI-native billing that runs in your own VPC.
