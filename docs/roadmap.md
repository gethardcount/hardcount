# Roadmap

## Recently shipped

July–August 2026:

- prepaid commits + drawdown
- provider cost tables with in-app per-customer margin
- team invites (ADR-081)
- ambiguous-charge safety (ADR-105, 107, 108)
- bad-debt semantics (ADR-110–113)
- a 30-check money-invariant sweep in CI
- multi-replica leader leases (ADR-114): every background job takes a per-tick lease that every claim re-checks. So a dead replica is replaced in seconds, and a transaction-mode pooler is safe.

Dated detail: [`CHANGELOG.md`](../CHANGELOG.md).

## Explicitly deferred (on hold pending design partner)

- Helm chart + Terraform AWS module. Multi-replica HA itself is no longer deferred: as of 2026-08-30, N ≥ 2 behind a load balancer is the supported production posture, and the engine is being built for it. See [`docs/dev/ha-readiness-2026-07-06.md`](dev/ha-readiness-2026-07-06.md).
- Stripe Billing migration tool (`velox-import`)
- SOC 2 / GDPR-deletion / audit-log retention enterprise-readiness docs
- RBAC / role enforcement. Invites shipped with every member holding full access; role-scoped permissions land when a design partner names the split. The SSO direction is predetermined: embedded OIDC/SAML in-process, never a SaaS auth vendor.
- Operator polish: bulk actions, billing alerts, plan-migration cohorts (each was removed before launch and will be rebuilt when asked for), and a docs site

These are paused, not killed. They land when a real customer names the specific shape they need; pre-launch builds optimise the wrong version of each.
