# Velox documentation

**New here? Read the [README](../README.md), then run the
[quick start](quick-start.md).**

## Try it

1. [README](../README.md): what Velox is, who it is for, and a demo.
2. [Quick start](quick-start.md): start Velox on your computer with only
   Docker, run the demo, and see one month of token usage become an
   invoice, its PDF and a margin report. Each step says what you see.
3. [Is Velox right for you?](../README.md#is-velox-right-for-you): who it fits, and the cases where
   another tool is the better choice.
4. [What's in the box](features.md): every feature, with what it does for you.
5. [How Velox compares](comparison.md) with four open-source billing
   engines, with sources, and [why Velox exists](why-velox.md).
6. [Roadmap](roadmap.md): what shipped recently and what is deferred.
   Every change, by date: [CHANGELOG](../CHANGELOG.md).

## Understand it

1. [How Velox works](how-velox-works.md): one picture and six steps, from
   a usage event to a paid invoice.
2. [Glossary](glossary.md): the words you will meet, plain meaning first.
3. [How much traffic Velox handles](scale.md): what was measured, and
   where one Postgres stops being enough.
4. [How Velox is engineered](ENGINEERING.md): the rules, with the proof
   for each.
5. [Card payment failure handling](payments.md): declines, ambiguous
   results, crashes mid-charge, duplicate webhooks, refunds, and the known
   gaps.
6. The measured runs in full:
   [correctness under failure](benchmarks/failure-correctness.md) and
   [sustained throughput](benchmarks/sustained-throughput.md).

## Connect your product

1. [Send usage and get your first invoice](first-invoice.md): create a
   customer, send token usage with curl or LiteLLM, and open the invoice,
   in about ten minutes. Start here.
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
   LiteLLM proxy without writing code.

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

---

Changing Velox itself? Contributor docs, design notes and records are in [docs/dev/](dev/README.md).
