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
  [Payments](concepts/payments.md) · [Tax](ops/tax-calculation.md) ·
  [Test clocks](concepts/test-clocks.md) · [Margin](concepts/margin.md) ·
  [Glossary](glossary.md).
- **Guides:** [Bill for AI tokens by model](guides/bill-for-ai-tokens.md) ·
  [AI pricing reference](ai-pricing.md) ·
  [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md) ·
  [Send usage from LiteLLM](integrations/litellm.md) ·
  [Connect Stripe](guides/connect-stripe.md) ·
  [Test a billing change](guides/test-a-billing-change.md) ·
  [Set up email](ops/email-setup.md) · [Manage API keys](api-keys.md).
- **API reference:** [The Hardcount API](api-surface.md) ·
  [Authentication and keys](api/authentication.md) ·
  [Errors](api/errors.md) · [Idempotency](api/idempotency.md) ·
  [Rate limits](api/rate-limits.md) · [Pagination](api/pagination.md) ·
  [Timing and limits](api/timing-and-limits.md) ·
  [Two calls in four languages](api/two-calls.md) ·
  [Webhooks](webhooks.md) · [Versioning](versioning.md) ·
  [Every route: the OpenAPI file](../api/openapi.yaml).
- **Self-host:** [Overview](self-host.md) ·
  [Production on one VM](production-one-vm.md) ·
  [High availability](ops/high-availability.md) ·
  [Settings](settings.md) · [Postgres](ops/postgres-requirements.md) ·
  [Backups](ops/backup-considerations.md) · [Upgrades](upgrades.md) ·
  [How much traffic it handles](scale.md) ·
  [Card payment failure handling](payments.md) ·
  [Runbook](ops/runbook/index.md).
- **Internals:** [Architecture](architecture.md) ·
  [How we test](testing.md) · [How Hardcount is engineered](ENGINEERING.md) ·
  Benchmarks: [correctness under failure](benchmarks/failure-correctness.md)
  and [sustained throughput](benchmarks/sustained-throughput.md) ·
  [Design decisions](adr/README.md) · [Records](dev/records/README.md).

The project: [What's in the box](features.md) ·
[Why Hardcount exists](why-hardcount.md) ·
[How Hardcount compares](comparison.md) · [Roadmap](roadmap.md) ·
[CHANGELOG](../CHANGELOG.md). Changing Hardcount itself? Contributor docs
are in [docs/dev/](dev/README.md).
