# Hardcount documentation

**New here? Read the [README](../README.md), then run the
[quick start](quick-start.md).**

## Try it

1. [Quick start](quick-start.md): start Hardcount on your computer with git,
   Docker and a bash shell, run the demo, and see one month of token usage
   become an invoice, its PDF and a margin report.
2. [What's in the box](features.md): every feature, with what it does for you.
3. [How Hardcount compares](comparison.md) with four open-source billing
   engines, with sources, and [why Hardcount exists](why-hardcount.md).
4. [Roadmap](roadmap.md): what is being built now, next and later, and
   what Hardcount will not build. Every change, by date:
   [CHANGELOG](../CHANGELOG.md).
5. [Versioning before 1.0](versioning.md): what a release can break, and
   where each breaking change is written down.

## Understand it

1. [How Hardcount works](how-hardcount-works.md): one picture and six steps, from
   a usage event to a paid invoice.
2. [Glossary](glossary.md): the words you will meet, plain meaning first.
3. [How much traffic Hardcount handles](scale.md): what was measured, and
   where one Postgres stops being enough.
4. [Card payment failure handling](payments.md): declines, ambiguous
   results, crashes mid-charge, duplicate webhooks, refunds, and the known
   gaps.

## Connect your product

1. [Send usage and get your first invoice](first-invoice.md): create a
   customer, send token usage with curl or LiteLLM, and open the invoice.
   Start here.
2. [AI pricing reference](ai-pricing.md): the models each recipe prices,
   pricing your own model, prepaid credit, and margin.
3. [API surface](api-surface.md): the core routes. The full reference is
   [`api/openapi.yaml`](../api/openapi.yaml).
4. [API keys](api-keys.md): get a key, send it, test and live mode, and
   rotation.
5. [Webhooks](webhooks.md): events, signatures, and retries.
6. [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md):
   poll a prepaid customer's live credit balance and gate in your own code.
7. [LiteLLM integration](integrations/litellm.md): send token usage from a
   LiteLLM proxy with a few lines of proxy config.

## Run it for real

1. [Self-host](self-host.md): what you need and how the pieces fit.
2. [Production on one VM](production-one-vm.md): install with Docker
   Compose, then HTTPS, email, Stripe, nightly backups and a go-live
   checklist.
3. [High availability](ops/high-availability.md): run two or more copies
   behind a load balancer, with health checks, shutdown and rolling upgrades.
4. [Postgres requirements](ops/postgres-requirements.md),
   [email setup](ops/email-setup.md),
   [backups](ops/backup-considerations.md) and
   [tax calculation](ops/tax-calculation.md).
5. When something breaks: the [operations runbook](ops/runbook/index.md).
   Find what you see, then follow its steps.

## Engineering

1. [How Hardcount is engineered](ENGINEERING.md): the rules, with the proof
   for each.
2. Measured runs: [correctness under failure](benchmarks/failure-correctness.md)
   and [sustained throughput](benchmarks/sustained-throughput.md).

---

Changing Hardcount itself? Contributor docs, design notes and records are in [docs/dev/](dev/README.md).
