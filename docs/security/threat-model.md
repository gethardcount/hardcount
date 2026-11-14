# Threat model

This page is for whoever evaluates or runs Hardcount. It says what Hardcount protects, which attacks it is built to stop and with which control, and which parts are your job. What data is stored, and how, is in [What data Hardcount stores](data.md). How to report a vulnerability: [SECURITY.md](../../SECURITY.md).

## What Hardcount protects

- **Correct money.** The right amount on each invoice, and no second charge for it ([How Hardcount keeps a bill right](../ENGINEERING.md)).
- **Accounts kept apart.** One account (tenant) never reads or changes another's data.
- **Credentials.** API keys, passwords, sessions, signing secrets and Stripe keys.
- **Personal data** of your customers and your team.

## Attacks and controls

| Attack | Control | Where | Status |
|---|---|---|---|
| One account reads another account's data | Row-level security in Postgres on most requests. Outside local, start-up stops if the request-time database user can bypass it. | [`cmd/hardcount/main.go`](../../cmd/hardcount/main.go), [`rls_isolation_test.go`](../../internal/platform/postgres/rls_isolation_test.go) | Partial: the listed paths that skip it filter by account in code, checked by review only ([Design rules](../architecture.md#design-rules)) |
| A stolen API key | Stored as a salted hash; tied to test or live mode; a usage-only key can only send usage; revoke, rotate and expiry; a rate limit per key | [`internal/auth/`](../../internal/auth/), [ADR-141](../adr/141-api-key-storage-and-rotation.md) | Partial: a secret key can do everything in its account and mode. No narrower scopes, no IP allow list. |
| A forged or replayed Stripe webhook | Stripe's signature check with the account's own secret, refusing signatures older than 5 minutes; the event's mode, and the Hardcount account it names, must match the endpoint's; each Stripe event is recorded once | [`internal/payment/handler.go`](../../internal/payment/handler.go), [`webhook_store.go`](../../internal/payment/webhook_store.go) | In place |
| A stolen database dump | Personal data, signing secrets and Stripe keys encrypted; passwords, keys, sessions and link tokens hashed; the keys live outside the database | [`internal/platform/crypto/`](../../internal/platform/crypto/) | Partial: addresses, some copies, and the links in emails not yet sent are plain ([Known gaps](data.md#known-gaps)) |
| A client that retries or sends a wrong request | `Idempotency-Key` replays the first answer for 24 hours and refuses the same key with a different body; each usage event's own key is kept for good; request validation; a 1 MB body limit (larger on the LiteLLM route) | [`idempotency.go`](../../internal/api/middleware/idempotency.go), [Idempotency](../api/idempotency.md) | In place |
| A compromised dependency or image | `govulncheck` on every pull request to `main`; Dependabot for GitHub Actions and base images (Go and npm packages are updated by hand); on `main` and release tags, images are pushed, then scanned with Trivy and Grype, an SBOM is made, and both images are signed with Cosign only if the scans pass | [`ci.yml`](../../.github/workflows/ci.yml), [`dependabot.yml`](../../.github/dependabot.yml) | Partial: an image that fails a scan stays published, unsigned. No security static analysis (SAST). |
| A cross-site request to the dashboard | A same-site check on every browser `POST`, `PUT`, `PATCH` and `DELETE` to sign-in and to the signed-in API, unless it carries an API key and no session cookie; an `HttpOnly`, `SameSite=Lax` cookie; the API's answers, and the dashboard image's pages, refuse to be framed | [`csrf.go`](../../internal/api/middleware/csrf.go), [`security.go`](../../internal/api/middleware/security.go), [`nginx-security-headers.conf`](../../deploy/compose/nginx-security-headers.conf) | In place |
| Password guessing | A rate limit per address on sign-in; repeated wrong passwords slow down; bcrypt; common leaked passwords refused | [`throttle.go`](../../internal/user/throttle.go), [Who can sign in](../production-one-vm.md#who-can-sign-in-to-the-dashboard) | Partial: no two-factor sign-in |
| A webhook URL aimed at your own network | Only `https://`, except `localhost` outside production; private, link-local and cloud metadata addresses refused everywhere, loopback refused in production; checked when the endpoint is saved and on every connection | [`internal/webhook/service.go`](../../internal/webhook/service.go) | In place |
| A forged Hardcount webhook at your receiver | Each event signed with HMAC-SHA256 and a timestamp | [`internal/webhook/service.go`](../../internal/webhook/service.go), [Receive webhooks](../guides/receive-webhooks.md) | In place |
| A link token leaked through logs | Tokens removed from the access log, traces and the Compose stack's nginx log | [`internal/api/router.go`](../../internal/api/router.go) | In place |

## Your job as the operator

- **TLS.** Hardcount speaks plain HTTP. End TLS in a proxy in front of it ([Production on one VM, step 3](../production-one-vm.md#3-add-https-with-caddy)).
- **Network exposure.** Decide who can reach the dashboard and the API. You can put sign-in behind your own SSO or VPN ([Who can sign in](../production-one-vm.md#who-can-sign-in-to-the-dashboard)).
- **The database.** Limit who can connect, encrypt its disk, and give requests the limited database user ([Postgres](../ops/postgres-requirements.md)).
- **Backups.** Back up the database and keep them as safe as the database ([Backups](../ops/backup-considerations.md)).
- **Keys.** Keep `HARDCOUNT_ENCRYPTION_KEY` and `HARDCOUNT_EMAIL_BIDX_KEY` in a secrets manager, apart from the backups. Hardcount cannot rotate them.
- **Traffic floods.** Hardcount limits requests per key and per address, with Redis. A flood is for your load balancer or firewall.
- **Logs.** They hold IP addresses and email recipients ([Logs](data.md#logs)). Set their retention and access.
- **Upgrades.** Only the latest release gets security fixes ([Supported versions](../../SECURITY.md#supported-versions)).
- **Erasure requests.** Hardcount has no built-in erasure ([Retention and deletion](data.md#retention-and-deletion)).

## Out of scope, or not done yet

- No external security audit or penetration test.
- No SOC 2 control mapping.
- No two-factor sign-in.
- Team roles are recorded but not enforced: every dashboard member can do everything.
- No API key scopes beyond the two key types, secret and usage-only ([Key types](../api/authentication.md#key-types)).
- No built-in rotation of the encryption key or the blind-index key.
- No security static analysis (SAST) in CI.
- Attacks on your own infrastructure, Stripe, or your email provider ([Out of scope](../../SECURITY.md#out-of-scope)).
