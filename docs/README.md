# Hardcount documentation

Hardcount is a billing system you run yourself. Your app sends usage, and
Hardcount prices it, makes the invoice and charges the card through Stripe.

**New here?** Read [How Hardcount works](how-hardcount-works.md), then run
the [quick start](quick-start.md).

- **Get started:** [How Hardcount works](how-hardcount-works.md) ·
  [Quick start](quick-start.md) ·
  [Send usage and get your first invoice](first-invoice.md).
- **Concepts:** [The data model](concepts/data-model.md) ·
  [Usage events](concepts/usage-events.md) ·
  [Meters and prices](concepts/meters-and-prices.md) ·
  [Subscriptions](concepts/subscriptions.md) ·
  [Invoices](concepts/invoices.md) · [Credits](concepts/credits.md) ·
  [Payments](concepts/payments.md) · [Test clocks](concepts/test-clocks.md) ·
  [Margin](concepts/margin.md) · [Glossary](glossary.md).
- **Guides:** [AI pricing reference](ai-pricing.md) ·
  [Bill for AI tokens by model](guides/bill-for-ai-tokens.md) ·
  [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md) ·
  [Send usage from LiteLLM](integrations/litellm.md) ·
  [Receive webhooks](guides/receive-webhooks.md) ·
  [Connect Stripe](guides/connect-stripe.md) ·
  [Test a billing change](guides/test-a-billing-change.md) ·
  [Manage API keys](api-keys.md) · [Set up tax](ops/tax-calculation.md).
- **API reference:** [The Hardcount API](api-surface.md) ·
  [Authentication and keys](api/authentication.md) ·
  [Errors](api/errors.md) · [Idempotency](api/idempotency.md) ·
  [Pagination](api/pagination.md) · [Versioning](versioning.md) ·
  [Rate limits](api/rate-limits.md) ·
  [Timing and limits](api/timing-and-limits.md) ·
  [Two calls in four languages](api/two-calls.md) ·
  [Every endpoint](../api/openapi.yaml)
  ([the OpenAPI file](https://raw.githubusercontent.com/gethardcount/hardcount/main/api/openapi.yaml)) ·
  [Webhooks](webhooks.md) · [Webhook events](api/webhook-events.md).
- **Self-host:** [Overview](self-host.md) ·
  [Production on one VM](production-one-vm.md) ·
  [The Compose files](../deploy/compose/README.md) ·
  [High availability](ops/high-availability.md) ·
  [Settings](settings.md) · [Postgres](ops/postgres-requirements.md) ·
  [Backups](ops/backup-considerations.md) · [Upgrades](upgrades.md) ·
  [How much traffic it handles](scale.md) ·
  [Monitoring](ops/runbook/monitoring.md) ·
  [Set up email](ops/email-setup.md) ·
  [Card payment failure handling](payments.md) ·
  [Runbook](ops/runbook/index.md).
- **Internals:** [Architecture](architecture.md) ·
  [How we test](testing.md) · [How Hardcount is engineered](ENGINEERING.md) ·
  Benchmarks: [correctness under failure](benchmarks/failure-correctness.md)
  and [sustained throughput](benchmarks/sustained-throughput.md) ·
  [Design decisions](adr/README.md) · [Records](dev/records/README.md).
- **Project:** [What's in the box](features.md) ·
  [How Hardcount compares](comparison.md) · [Roadmap](roadmap.md) ·
  [CHANGELOG](../CHANGELOG.md).
- **Contribute** (changing Hardcount itself):
  [Guides for contributors](dev/README.md) ·
  [Contributing](../CONTRIBUTING.md).
