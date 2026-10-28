# Security Policy

## Reporting a vulnerability

Use GitHub's **[private vulnerability reporting](https://github.com/gethardcount/hardcount/security/advisories/new)** (Security tab → Report a vulnerability) — reports reach the maintainer privately, with no email required. If you prefer email: **sagar@sagarwaidande.org**.

Please do **not** open public GitHub issues for suspected vulnerabilities.

We commit to:

| Stage | Target |
|---|---|
| Acknowledge receipt | within 2 business days |
| Initial triage + severity assessment | within 5 business days |
| Patch landed in main | within 30 days for high/critical, 90 days for medium, best-effort for low |
| Public disclosure (with credit) | after a fixed release is available, coordinated with the reporter |

## Supported versions

Hardcount is pre-1.0. Only the latest release gets security fixes.

- A fix lands in `main` and ships in the next release, a patch or a minor release.
- Older releases get no backported fixes. To get a fix, upgrade to the latest release.
- The next release, 0.9.0, is the first named Hardcount (the project was called Velox). It cannot take over a database Velox made, from 0.8.x or from a build of `main`: you install it fresh. So on 0.8.x, a fix released after 0.8.2 needs a fresh install.
- Before 1.0, an upgrade can need more than a restart: read **Before upgrading** in the [CHANGELOG](CHANGELOG.md) first. See [Versioning before 1.0](docs/versioning.md).

## In scope

- The Hardcount Go binary (`cmd/hardcount`, `cmd/hardcount-bootstrap`, `cmd/hardcount-bench-seed`, `cmd/hardcount-doctor`)
- Code under `internal/` and `pkg/`
- The migration runner and schema in `internal/platform/migrate/`
- The web-v2 dashboard (`web-v2/`)
- Docker Compose deployment config under `deploy/compose/`
- Outbound webhook signing, inbound Stripe webhook verification, API key handling, session/cookie auth, RLS (row-level security) policy enforcement, AES-GCM encryption-at-rest, HMAC blind index for email (a keyed hash that allows equality lookup without storing the plaintext address)

## Out of scope

- Vulnerabilities in the operator's deployment environment (Kubernetes cluster, managed Postgres, load balancer, secrets store, IAM)
- Vulnerabilities in third-party services that Hardcount integrates with (Stripe, cloud providers, email providers, S3, KMS) — report those to the vendor
- Configuration mistakes by the operator (e.g., running with `HARDCOUNT_ENCRYPTION_KEY` unset in a non-production env, or exposing the dashboard without TLS termination)
- DoS via traffic flooding (a property of the operator's load balancer + WAF, not Hardcount)
- Self-XSS, social engineering, physical attacks
- Reports against forks or vendored copies of Hardcount — please reproduce on the canonical `main` branch first

## Hardening status

Hardcount is **pre-launch** and pre-audit. Encryption-at-rest, RLS on most API requests (the exceptions are listed in [Architecture](docs/architecture.md#design-rules)), audit log immutability, HMAC webhook signing, bcrypt (cost 12) passwords, SHA-256 session/API-key/token hashing, security headers, GCRA rate limiting (a leaky-bucket-style algorithm), and TLS-only intent (secure cookies + HSTS; TLS termination itself is left to the operator's proxy) are all implemented. (A SOC 2 control mapping waits until a design partner requires it: see [Later](docs/roadmap.md#later) in the roadmap.)

Known gaps, documented openly:

- No built-in mechanism to rotate `HARDCOUNT_ENCRYPTION_KEY` or `HARDCOUNT_EMAIL_BIDX_KEY` (a rebuild on envelope encryption — data keys wrapped under a rotatable master key — is planned)
- No MFA on dashboard login (no MFA in v1; SSO direction is embedded OIDC/SAML per ADR-014 — Hardcount will not depend on a SaaS auth vendor)
- No SAST (static application security testing) in CI (Semgrep / CodeQL planned)
- No threat model document (STRIDE / LINDDUN, two threat-modeling frameworks, planned)
- No external penetration test on record yet

If you can help close any of these, contributions are welcome via the normal PR process.

## Safe-harbor

We will not pursue legal action against good-faith security research that:

- Does not access, modify, or destroy data belonging to anyone other than the researcher
- Does not degrade availability for other users
- Stays within the in-scope list above
- Coordinates disclosure with us before going public

If a researcher inadvertently violates this policy in good faith, please tell us promptly and we'll work it out.
