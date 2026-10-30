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
- 0.9.0 is the first release named Hardcount (the project was called Velox). It cannot take over a database Velox made, from 0.8.x or from a build of `main`. So on 0.8.x, every later fix needs a fresh install.
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

Hardcount has had no external security audit or penetration test yet.

In place:

- **Account isolation:** row-level security on most API requests; the exceptions are listed in [Architecture](docs/architecture.md#design-rules). Hardcount refuses to start if its request-time database user can bypass it.
- **Secrets at rest:** AES-256-GCM encryption for Stripe keys, webhook signing secrets and customer personal data, required in production; an HMAC blind index (a keyed hash) to find a customer by email without storing the address in plain text.
- **Credentials:** passwords as bcrypt (cost 12); sessions, API keys and one-time tokens stored only as SHA-256 hashes.
- **Sign-in** ([who can sign in](docs/production-one-vm.md#who-can-sign-in-to-the-dashboard)):
  - about 30,000 common leaked passwords are refused when a password is set;
  - repeated wrong passwords slow down instead of locking the account;
  - the sign-in cookie travels only over HTTPS in production.
- **Requests:** CSRF checks on the dashboard, security headers and HSTS, rate limiting. TLS itself is ended by the operator's proxy.
- **Webhooks:** outbound events signed with HMAC-SHA256; inbound Stripe events verified.
- **Audit log:** append-only; a database trigger refuses any update or delete.
- **Supply chain:** CI runs `govulncheck` and scans the images with Trivy; release images carry an SBOM and are signed.

Known gaps:

- **No two-factor sign-in.** It is built when an operator asks for it or a customer's security review requires it ([ADR-095](docs/adr/095-login-security-roadmap-and-mfa.md)). Until then, a team can put the dashboard's sign-in behind its own SSO or VPN.
- **No built-in rotation** of `HARDCOUNT_ENCRYPTION_KEY` or `HARDCOUNT_EMAIL_BIDX_KEY`.
- **No static analysis** (SAST, such as CodeQL or Semgrep) in CI.
- **No threat model document.**
- **No external penetration test** on record.
- **No SOC 2 control mapping** until a customer requires one ([Later](docs/roadmap.md#later) in the roadmap).

If you can help close any of these, contributions are welcome via the normal PR process.

## Safe-harbor

We will not pursue legal action against good-faith security research that:

- Does not access, modify, or destroy data belonging to anyone other than the researcher
- Does not degrade availability for other users
- Stays within the in-scope list above
- Coordinates disclosure with us before going public

If a researcher inadvertently violates this policy in good faith, please tell us promptly and we'll work it out.
