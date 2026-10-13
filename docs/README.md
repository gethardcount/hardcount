# Velox documentation

Start here to find the right doc. Pick what you want to do, then read the
docs in the order listed. Words you may not know are in the
[glossary](glossary.md).

## Decide if Velox fits

1. [README](../README.md): what Velox is, who it is for, and a demo.
2. [What Velox is not](../README.md#what-velox-is-not): the cases where
   another tool is the better choice.
3. [What's in the box](features.md): every feature, in one list.
4. [How Velox compares](comparison.md) with four open-source billing
   engines, with sources, and [why Velox exists](why-velox.md).
5. [How Velox is engineered](ENGINEERING.md): the principles, one page.
6. [Will it take our volume?](scale.md): what was measured, and where one
   Postgres stops being enough.

## Try it on your machine

1. [Quick start](../README.md#quick-start): Postgres, the API and the
   dashboard on localhost.
2. [`scripts/demo.sh`](../scripts/demo.sh): the full flow from usage events
   to a finalized invoice, its PDF and a margin report.
3. [AI pricing in code](ai-pricing.md): one meter, a prepaid commit and a
   margin read, in five steps.

## Connect your product

1. [API surface](api-surface.md): the core routes. The full reference is
   [`api/openapi.yaml`](../api/openapi.yaml).
2. [API keys](api-keys.md): key types, rotation, and adding tenants.
3. [Webhooks](webhooks.md): events, signatures, and retries.
4. [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md):
   poll a prepaid customer's live credit balance and gate in your own code.
5. [LiteLLM integration](integrations/litellm.md): send token usage from a
   LiteLLM proxy without writing code.

## Run it in production

1. [Self-host](self-host.md): what you need and how the pieces fit.
2. [Docker Compose on one VM](../deploy/compose/README.md): step-by-step
   install.
3. [Deployment](../deploy/README.md): scaling, upgrades, and shutdown.
4. [Postgres requirements](ops/postgres-requirements.md),
   [email setup](ops/email-setup.md),
   [backups](ops/backup-considerations.md) and
   [tax calculation](ops/tax-calculation.md).
5. When something breaks: the [operations runbook](ops/runbook.md), the
   [leader-lease runbook](ops/runbook-leader-leases.md) and the
   [failure-mode catalog](failure-modes/README.md).
6. [Stripe end-to-end test](ops/stripe-end-to-end-test.md): check a real
   Stripe account before go-live.

## Contribute

1. [CONTRIBUTING](../CONTRIBUTING.md): setup, tests, and what CI checks.
2. [Architecture](architecture.md): package layout and design rules.
3. [Money-path robustness playbook](dev/money-path-robustness-playbook.md):
   required for any change to invoices, payments, credits, dunning,
   subscriptions or tax.
4. [Manual-test strategy](dev/manual-test-strategy.md) and
   [MANUAL_TEST](../MANUAL_TEST.md): how flows are checked by hand.
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
- [Product stability plan](dev/product-stability-plan.md) and
  [manual-test findings](dev/manual-test-findings.md)
- [CHANGELOG](../CHANGELOG.md)
