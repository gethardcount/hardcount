# Velox documentation

Start here to find the right doc. Pick what you want to do, then read the
docs in the order listed. Words you may not know are in the
[glossary](glossary.md), with plain meanings first.

## Decide if Velox fits

1. [README](../README.md): what Velox is, who it is for, and a demo.
2. [How Velox works](how-velox-works.md): one picture and six steps, from
   a usage event to a paid invoice.
3. [What Velox is not](../README.md#what-velox-is-not): the cases where
   another tool is the better choice.
4. [What's in the box](features.md): every feature, in one list.
5. [How Velox compares](comparison.md) with four open-source billing
   engines, with sources, and [why Velox exists](why-velox.md).
6. [How Velox is engineered](ENGINEERING.md): the principles, one page.
7. [Will it take our volume?](scale.md): what was measured, and where one
   Postgres stops being enough.

## Try it on your machine

1. [Quick start](quick-start.md): start Velox on your computer with only
   Docker, run the demo, and see one month of token usage become an
   invoice, its PDF and a margin report. Each step says what you see.

## Connect your product

1. [Send usage and get your first invoice](first-invoice.md): create a
   customer, send token usage with curl or LiteLLM, and open the invoice,
   in about ten minutes. Start here.
2. [AI pricing reference](ai-pricing.md): the models each recipe prices,
   pricing your own model, prepaid credit, and margin.
3. [API surface](api-surface.md): the core routes. The full reference is
   [`api/openapi.yaml`](../api/openapi.yaml).
4. [API keys](api-keys.md): key types, rotation, and adding tenants.
5. [Webhooks](webhooks.md): events, signatures, and retries.
6. [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md):
   poll a prepaid customer's live credit balance and gate in your own code.
7. [LiteLLM integration](integrations/litellm.md): send token usage from a
   LiteLLM proxy without writing code.

## Run it in production

1. [Self-host](self-host.md): what you need and how the pieces fit.
2. [Docker Compose on one VM](../deploy/compose/README.md): step-by-step
   install.
3. [Production on one VM](production-one-vm.md): HTTPS, email, Stripe,
   nightly backups, a go-live checklist and upgrades, for a small team.
4. [High availability](ops/high-availability.md): run two or more copies
   behind a load balancer, with health checks, shutdown and rolling upgrades.
5. [Postgres requirements](ops/postgres-requirements.md),
   [email setup](ops/email-setup.md),
   [backups](ops/backup-considerations.md) and
   [tax calculation](ops/tax-calculation.md).
6. When something breaks: the [operations runbook](ops/runbook.md), the
   [leader-lease runbook](ops/runbook-leader-leases.md) and the
   [failure-mode catalog](failure-modes/README.md).

## Contribute

1. [CONTRIBUTING](../CONTRIBUTING.md): setup, tests, and what CI checks.
2. [Architecture](architecture.md): package layout and design rules.
3. [Money-path robustness playbook](dev/money-path-robustness-playbook.md):
   required for any change to invoices, payments, credits, dunning,
   subscriptions or tax.
4. [How we test](testing.md): which test to write, and how to run it.
   The [release checklist](dev/release-checklist.md) holds the live checks
   run by hand before each release, and the
   [Stripe end-to-end test](dev/stripe-end-to-end-test.md) has the
   step-by-step Stripe commands.
5. [OpenAPI workflow](dev/openapi-workflow.md): changing the API contract.
6. [Writing style](dev/writing-style.md): how to write docs here.
7. AI coding agents start at [CLAUDE.md](../CLAUDE.md), then
   [agent prompting standards](dev/agent-prompting-standards.md).

## Understand a decision

- [The evidence behind the engineering principles](engineering-evidence.md).
- [Correctness under failure](benchmarks/failure-correctness.md) and
  [Sustained throughput](benchmarks/sustained-throughput.md): the measured
  runs in full.
- [Roadmap](roadmap.md): what shipped recently and what is deferred.

- [Architecture decision records (ADRs)](adr/README.md): one decision per
  file, with the reasons and the alternatives.
- Design docs, written while each feature was planned. Where a design doc
  and the code disagree, the code and the ADRs are the current record.
  - [Billing thresholds](design-billing-thresholds.md)
  - [CC on billing emails](design-cc-emails.md)
  - [Create-preview endpoint](design-create-preview.md)
  - [Customer usage endpoint](design-customer-usage.md)
  - [Multi-dimensional meters](design-multi-dim-meters.md)
  - [Pricing recipes](design-recipes.md)
  - [Provider cost tables](design-cost-tables.md)
  - [Prepaid commits](design-prepaid-commits.md)
  - [Paid-commit credit-note relief](design-commit-cn-relief.md)

## Records

Dated documents. They describe the state on their date; the current truth
is in the code and the docs above.

- [Full-product audit, 2026-07-02](dev/audit-2026-07-02-full-product.md) and
  its [remediation plan](dev/audit-2026-07-02-remediation-plan.md)
- [HA readiness, 2026-07-06](dev/ha-readiness-2026-07-06.md)
- [Design review, 2026-07-10](dev/design-review-2026-07-10.md)
- [Product stability plan](dev/product-stability-plan.md)
- [CHANGELOG](../CHANGELOG.md)
