# Contributor docs

For people changing Velox itself. You do not need these pages to use or run
Velox: the user and operator docs start at [docs/README.md](../README.md).

## Start here

1. [CONTRIBUTING](../../CONTRIBUTING.md): setup, tests, and what CI checks.
2. [Architecture](../architecture.md): package layout and design rules.
3. [Money-path robustness playbook](money-path-robustness-playbook.md):
   required for any change to invoices, payments, credits, dunning,
   subscriptions or tax.
4. [How we test](../testing.md): which test to write, and how to run it.
   The [release checklist](release-checklist.md) holds the live checks
   run by hand before each release, and the
   [Stripe end-to-end test](stripe-end-to-end-test.md) has the
   step-by-step Stripe commands.
5. [OpenAPI workflow](openapi-workflow.md): changing the API contract.
6. [Writing a database migration](migrations.md): changing the
   database schema.
7. [Writing style](writing-style.md): how to write docs here.
8. [Contributor glossary](glossary.md): words from the code, the database
   and the operations docs.
9. AI coding agents start at [CLAUDE.md](../../CLAUDE.md).

## Background

Background for a decision, kept as written.

- [Architecture decision records (ADRs)](../adr/README.md): one decision per
  file, with the reasons and the alternatives.
- [Design docs](design/README.md): the plan for each feature, written
  before it was built. Where a design doc and the code disagree, the code
  and the ADRs are the current record.
- [Records](records/README.md): dated audits, reviews, plans and benchmark
  run journals, kept as written.
- [CHANGELOG](../../CHANGELOG.md): every change, by date.
