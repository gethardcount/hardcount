# What's in the box

Everything below ships in the one MIT edition. For the cases Velox does not cover, see [What Velox is not](../README.md#what-velox-is-not).

## AI/usage-native

- **Multi-dimensional meters**: one meter, N pricing rules, dimensions on every event.
- **LiteLLM drop-in**: the proxy spend callback becomes dimensioned token events, with idempotent replay dedupe. No SDK, no schema work.
- **Prepaid commits + drawdown**: sell "pay $9k, get $10k" as an invoice line. When the invoice finalizes, the balance is funded in one atomic step. Usage then draws it down (drawdown), promotional credits first ([ADR-078](adr/)).
- **Per-customer margin in-app**:
  - Maintain provider rates once (`/v1/provider-costs`).
  - Every event is stamped with its cost at ingest.
  - `GET /v1/customers/{id}/margin` answers "which customers lose us money?" Revenue it can't attribute goes to an explicit `unattributed_revenue` bucket ([ADR-079](adr/)).
- **Decimal quantities & rates**: `NUMERIC(38,12)` for fractional GPU-hours and partial tokens. Per-unit prices are decimal, so $3.00 / 1M tokens bills exactly, while invoice totals stay whole cents.
- **Per-rule aggregation modes**: `sum`, `count`, `last_during_period`, `last_ever`, `max`.
- **Pricing recipes**: one call instantiates products + prices + meters + dunning (`anthropic_style`, `openai_style`, `replicate_style`).
- **Customer cost visibility**: a per-customer usage/cost breakdown in the dashboard. There is also a token-authenticated public JSON endpoint your app can render ("$4.31 of GPT-4 today", with a projected bill). A packaged embeddable widget is roadmap, not shipped.

## Self-host first

- **One application process**: a single Go binary.
  - No queue broker, no worker fleet, no separate scheduler. Background jobs are goroutines and the job queue is a Postgres table, so there is one thing to deploy, restart and reason about.
  - The Compose file adds Postgres, Redis (distributed rate limiting only), the static dashboard and an nginx front door. That makes five containers, one of which is Velox.
- **MIT, and nothing is gated**: no licence key, no premium tier, no feature that unlocks when you pay.
  - That includes dunning, the feature that recovers your failed payments.
  - This is a lasting commitment: the engine stays MIT, and nothing that exists today will ever move behind a key.
  - Check this against whatever else you are evaluating. Open-core billing engines commonly put dunning, SSO and RBAC behind a key. Velox has no SSO or RBAC to gate (see [What Velox is not](../README.md#what-velox-is-not)).
- **Nothing phones home**: no licence check, no telemetry, no analytics SDK. OpenTelemetry tracing exists and exports to *your* collector when you configure one.
- **Data sovereignty**: customer billing data never leaves your infrastructure. This transfers the compliance obligations to you; it does not remove them. "Self-hosted so it's compliant by default" is not a claim we'll make.
- **Append-only audit log**: database triggers refuse any update or delete of an audit row.
- **Row-Level Security**: the database keeps test data apart from live data, and one tenant's rows apart from another's.

## Stripe-grade primitives (already shipped)

- **Subscriptions**: trial state machine with atomic flips · pause-collection · scheduled cancellation · plan changes with proration (you're only charged for the part of the period each price was active).
- **Pricing & discounts**: per-customer price overrides · prepaid credit ledger drawn against usage. Coupons were deliberately cut, because AI-native peers converge on credits, not promo codes ([ADR-039](adr/)).
- **Invoicing & collection**: PDF invoices · hosted invoice page with secure tokens · branded emails · dunning with a circuit breaker · invoice preview (subtotal only: no tax or credits).
- **Billing thresholds**: set a level on spend or on usage quantity, and Velox issues an invoice early when the period's running total reaches it. It fires once per period and is checked by the hourly billing run. It bills earlier; it does not stop usage.
- **Credits & refunds**: event-sourced credit ledger · a live balance read (credit left after usage not yet invoiced, for [stopping usage when credit runs out](stop-usage-when-credit-runs-out.md)) · credit notes (the formal "we owe you" document) with refunds.
- **Reliability & observability**:
  - Idempotency keys: retrying a request can never double-charge.
  - Transactional outbox: outbound webhook events commit in the same database transaction as the change that caused them, then a dispatcher delivers them. Customer emails are queued post-commit and delivered at-least-once from the queue.
  - Webhook signing with a 72h dual-signing rotation grace: during the window, both the old and the new secret sign every delivery.
  - A live webhook event tail with per-attempt delivery timelines.
  - Test clocks that simulate months of billing in seconds.

See [`CHANGELOG.md`](../CHANGELOG.md) for the full ship log.
