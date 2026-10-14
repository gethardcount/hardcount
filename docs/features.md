# What's in the box

Everything below ships in the one MIT edition. For the cases Velox does not cover, see [What Velox is not](../README.md#what-velox-is-not).

## AI/usage-native

- **Multi-dimensional meters**: one meter can have many pricing rules, and each event can carry dimensions (such as `model`) that pick the rule.
- **LiteLLM**: point LiteLLM's spend callback at Velox, and each call becomes token events with `model` and `provider` dimensions. A replayed call is counted once. No SDK is needed. It needs a `tokens` meter (the AI recipes create one) and customers whose external ID matches LiteLLM's `user` ([guide](integrations/litellm.md)).
- **Prepaid commits + drawdown**: sell "pay $9k, get $10k" as a line on a one-off invoice. When the invoice finalizes, the credit is granted in the same transaction, before the invoice is paid. Each later invoice for the customer draws it down when it is issued, using promotional credit first ([ADR-078](adr/078-prepaid-commits-phase-1.md)).
- **Per-customer margin in-app**:
  - Maintain provider rates once (`/v1/provider-costs`).
  - At ingest, each event gets its cost from the rate table when a rate matches its provider, model and token type. Events with no matching rate get no cost, and the margin report counts them.
  - `GET /v1/customers/{id}/margin` compares one customer's usage revenue with its provider cost. Base fees, credits and taxes are left out. Usage revenue that can't be matched to a model goes in `unattributed_revenue_cents` ([ADR-079](adr/079-provider-cost-tables.md)).
- **Decimal quantities & rates**: quantities are `NUMERIC(38,12)`, for fractional GPU-hours and partial tokens. Per-unit prices are stored as exact decimals, so a rate of $3.00 per 1M tokens is kept as written. Each invoice line is then rounded to whole cents.
- **Per-rule aggregation modes**: `sum`, `count`, `last_during_period`, `last_ever`, `max`.
- **Pricing recipes**: one call instantiates products + prices + meters + dunning (`anthropic_style`, `openai_style`, `replicate_style`).
- **Customer cost visibility**: a per-customer usage/cost breakdown in the dashboard. There is also a public JSON endpoint, opened with a per-customer token, that your app can render ("$4.31 of GPT-4 this billing period"). It shows usage charges so far this period and the customer's credit balance. A packaged embeddable widget is roadmap, not shipped.

## Self-host first

- **One application process**: a single Go binary.
  - No queue broker, no worker fleet, no separate scheduler. Background jobs are goroutines in the same process. Pending webhook and email deliveries wait in Postgres tables, so there is one thing to deploy and restart.
  - The Compose file adds Postgres, Redis (distributed rate limiting only), the static dashboard and an nginx front door. That makes five containers, one of which is Velox.
- **MIT, and nothing is gated**: no licence key, no premium tier, no feature that unlocks when you pay.
  - That includes dunning, the feature that recovers your failed payments.
  - Every release so far is MIT, and an MIT release stays MIT. The maintainers do not plan to move any existing feature behind a key.
  - Check this against whatever else you are evaluating. For example, Lago's automatic dunning is a [premium add-on](https://getlago.com/docs/guide/dunning/automatic-dunning). Velox has no SSO or RBAC to gate (see [What Velox is not](../README.md#what-velox-is-not)).
- **Nothing phones home**: no licence check, no telemetry, no analytics SDK. OpenTelemetry tracing exists and exports to *your* collector when you configure one.
- **Data sovereignty**: Velox stores customer billing data in your own Postgres. It sends data out only to services you configure, such as Stripe for payments and tax, and your SMTP server for email. This transfers the compliance obligations to you; it does not remove them. "Self-hosted so it's compliant by default" is not a claim we'll make.
- **Append-only audit log**: database triggers refuse any update or delete of an audit row.
- **Row-Level Security**: the database keeps test data apart from live data, and one tenant's rows apart from another's.

## Billing building blocks

- **Subscriptions**: trial state machine with atomic flips · pause-collection · scheduled cancellation · plan changes with proration (you're only charged for the part of the period each price was active).
- **Pricing & discounts**: per-customer price overrides · prepaid credit ledger drawn against usage. Coupons were cut before launch. Discounts use credits and per-customer price overrides instead ([ADR-039](adr/039-cut-coupons-pre-launch.md)).
- **Invoicing & collection**: PDF invoices · hosted invoice page with secure tokens · branded emails · dunning with a circuit breaker · invoice preview (subtotal only: no tax or credits).
- **Billing thresholds**: set a level on spend or on usage quantity, and Velox issues an invoice early when the period's running total reaches it. It fires at most once per billing period. With `reset_billing_cycle`, a fire starts a new period, so it can fire again. Velox checks thresholds on each billing run: every hour, or every 5 minutes when `APP_ENV=local`. It bills earlier; it does not stop usage.
- **Credits & refunds**: event-sourced credit ledger · a live balance read (credit left after usage not yet invoiced, for [stopping usage when credit runs out](stop-usage-when-credit-runs-out.md)) · credit notes (the formal "we owe you" document) with refunds.
- **Reliability & observability**:
  - Idempotency keys: send an `Idempotency-Key` header on a POST, PUT or PATCH, and a retry within 24 hours returns the first response instead of running the request again. A server error stays cached too (except on usage ingest, where a replay is deduplicated), so a retry cannot repeat a charge that may already have gone through.
  - Webhook outbox: every outbound webhook event is written to a Postgres table, and a dispatcher delivers it from there with retries. Some events are written in the same transaction as the change: `invoice.paid`, `payment.succeeded`, `payment.failed`, the credit-balance events, `dunning.started`, and subscription `created`, `activated`, `trial_ended` and `canceled`. The rest are written just after the change commits. If that write fails, Velox logs an error and the event is not sent. Customer emails are queued post-commit and delivered at-least-once from the queue.
  - Webhook signing with a 72h dual-signing rotation grace: during the window, both the old and the new secret sign every delivery.
  - A live webhook event tail with per-attempt delivery timelines.
  - Test clocks move a test customer forward through months of billing without waiting for real time.

See [`CHANGELOG.md`](../CHANGELOG.md) for the full ship log.
