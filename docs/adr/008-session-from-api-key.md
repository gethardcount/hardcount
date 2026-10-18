# ADR-008: Dashboard Session Cookies Minted From API Keys

**Date:** 2026-04-29
**Status:** Superseded by ADR-011 (session-from-API-key mechanism replaced by email/password sessions)

## Status
Superseded by [ADR-011](./011-email-password-auth-and-clean-api-keys.md) — the httpOnly cookie shape and `internal/session` package are kept, but sessions now bind to `users.id` (not `api_keys.id`) and are minted by `POST /v1/auth/login` after email + password verification. The XSS-mitigation rationale below still holds; only the credential at the front of the funnel changed.

## Date
2026-04-29

## Summary

Superseded by [ADR-011](011-email-password-auth-and-clean-api-keys.md). This ADR kept the API key as the credential but moved the browser to a short-lived httpOnly `velox_session` cookie, minted by `POST /v1/auth/exchange`. That way XSS could not read the key, and logout could revoke the session on the server. ADR-011 kept the cookie and the `internal/session` package, but binds sessions to `users.id` and mints them at `POST /v1/auth/login` after an email and password check. The XSS reasoning below still applies.

## Context

ADR-007 reverted the dashboard from email+password sessions to bearer-key auth: the operator pastes a `vlx_secret_…` key into `/login`, the browser stores it in `localStorage`, and every request rides `Authorization: Bearer <key>`. That correctly aligned with `project_auth_decision.md` ("API keys custom-built; user auth via WorkOS/Clerk later") and removed ~2k LOC of password / session / invitation surface.

A security review of the result surfaced concerns the bare-Bearer-localStorage shape could not answer:

- **XSS reads the credential.** `localStorage.getItem('velox_api_key')` is JS-reachable. Any successful cross-site scripting — malicious npm dependency, browser extension, future template injection — exfiltrates a credential that grants full tenant powers.
- **No server-side revocation.** Logout only clears the browser; the key itself stays valid for anyone who has it. Compare to a session row whose `revoked_at` makes the credential unusable for everyone.
- **No automatic expiry.** Browser sessions on the previous email+password design expired in 30 days; bearer keys live until manually revoked.
- **Persistence.** `localStorage` survives browser close / restart. A shared machine retains the credential indefinitely.
- **Blast radius.** A single compromised browser surface yields full-tenant write access; with a session, the same compromise can issue requests in that tab but cannot exfiltrate the credential.

ADR-007 named these and punted to a "trigger to revisit" — the first design partner that demanded real user accounts. The trigger is too late: the user-asked-for-discomfort surfaces *before* DP outreach, and a security questionnaire two weeks before close is a bad time to redesign the auth surface.

The fix is to keep the API key as the durable credential (the v1 plan from `project_auth_decision.md`) but project it onto a short-lived httpOnly session cookie for browser-side authentication.

## Decision

Reintroduce server-side session storage scoped to API keys (not users):

- New table `dashboard_sessions` — migration `0066_dashboard_sessions.up.sql`. Schema mirrors the legacy `sessions` table from migration `0034` minus the `user_id` FK; rows reference `api_keys` instead.
- New package `internal/session/` — `Service.Issue / Resolve / Revoke / RevokeAllForKey`, postgres-backed `Store`, `MiddlewareOrAPIKey` that accepts the cookie OR a Bearer header (cookie wins on conflict), `Handler` for `POST /v1/auth/exchange` and `POST /v1/auth/logout`.
- Cookie attributes: `velox_session`, `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` in staging/production. 7-day TTL (vs the 30-day on the deleted email+password design — shorter because the underlying credential is durable, the session is just the browser-side artefact of "I pasted that key recently").
- `POST /v1/auth/exchange` validates the pasted key via `auth.Service.ValidateKey`, mints a session row, and sets the cookie. Response body returns `{tenant_id, key_id, key_type, livemode, expires_at}` so the dashboard can populate AuthContext without a follow-up `/v1/whoami` round-trip.
- `POST /v1/auth/logout` revokes the session row and clears the cookie. Idempotent.
- `/v1/*` middleware switches from `auth.Middleware` (bearer-only) to `session.MiddlewareOrAPIKey` (cookie OR bearer). Cookie takes precedence so a stale Bearer header can't bypass session revocation.
- Frontend: `Login.tsx` posts to `/v1/auth/exchange`; the cookie attaches automatically via `credentials: 'include'`. `localStorage` no longer holds the key. `lib/api.ts` drops the Authorization header path.

This is a refinement of ADR-007, not a flip-flop. The credential model is unchanged — API keys, no user accounts, no password reset, no invitations. Only the browser-side artefact changes.

## Consequences

### Positive

- **httpOnly defeats XSS exfiltration.** A successful XSS in the dashboard can issue requests inside the current tab (it always could) but cannot read the cookie value.
- **Server-side revocation works.** Logout updates `dashboard_sessions.revoked_at`; the next request with that cookie 401s regardless of which device is using it.
- **Auto-expiry.** Sessions die after 7 days even if the operator never logs out. The underlying API key stays valid; the operator pastes it again.
- **Smaller blast radius if a browser is compromised.** The compromise yields request-issuing capability in the open tab, not the credential itself.
- **API SDK callers unaffected.** `MiddlewareOrAPIKey` accepts a Bearer header as the fallback path. curl, integration scripts, server-to-server calls — all work unchanged.
- **Cheap.** ~half a day to write, +1 migration, +1 small package (~250 LOC counting the handler). Re-uses the legacy `sessions` table's design lessons (sha256-at-rest, indexed on active rows) without resurrecting the user-account surface that lived alongside it.

### Negative

- **Re-introduces session storage code we just deleted.** Different shape (key-keyed, not user-keyed) but conceptually similar. CHANGELOG carries the explicit "this is a refinement of the auth-revert" framing so future readers don't assume churn.
- **Two auth code paths to maintain.** `MiddlewareOrAPIKey` branches on cookie-vs-Bearer. The branching is small and tested, but it's strictly more code than the bearer-only path of the previous commit.
- **Cookie hygiene now matters.** `Secure` flag in production, `SameSite` choice, cookie domain in multi-environment deploys. These were settled state in the email+password design; we re-inherit them.

### Trade-offs

- Trades a few hundred lines of session-table code for materially better security posture before DP outreach. The realistic alternative — wait until DP #1 demands it — would mean redesigning the auth surface during a sales cycle, which is the worst possible time.
- Keeps `internal/dashauth/` deleted (no email+password, no password reset, no invites). Only `internal/session/` returns, in a leaner shape.

## Alternatives Considered

- **Stay with Bearer-in-localStorage.** Rejected — concerns enumerated above. Acceptable at zero customers, untenable at first DP.
- **`sessionStorage` instead of `localStorage`.** Considered as a cheap improvement: clears on tab close, doesn't help with XSS exfiltration. Real win on the persistence axis but the XSS gap is the bigger concern. If we're touching the auth code, do it right.
- **Encrypt the key at rest in `localStorage` with a key derived from a pasted passphrase.** Rejected — the passphrase is also JS-reachable in memory. Theatre, not security.
- **Restrict the dashboard to publishable keys only; require a separate Bearer header for write actions.** Considered. Real defense, real UX cost (every mutation prompts for the secret key), and it doesn't address persistence or revocation. A future enhancement on top of session-from-key, not a replacement.
- **Migrate to Zitadel / WorkOS now.** Rejected per `feedback_pre_launch_scoping.md`. Adds an SSO-shaped service before any DP has named the integration shape they want; the right time is when the trigger fires and a real customer profile is in the room. Session-from-key is the bridge that keeps optionality open.

## Trigger to revisit

This shape stays valid until **any one** of:

- A design partner demands real user accounts (multi-human attribution, SSO, MFA, SAML).
- A tenant's security review specifically requires the credential to be *not* a long-lived API key (e.g., short-lived OIDC tokens with refresh).
- More than one human shares a tenant, and named-actor audit attribution becomes load-bearing.

When the trigger fires, evaluate Zitadel and WorkOS first. The session-from-key surface added here is intentionally narrow enough that swapping the credential model (key → OIDC) is straightforward — `auth.Service.ValidateKey` becomes `oidc.ValidateToken`, the rest of the session-handling stack carries.

## Related

- `docs/adr/007-revert-to-api-key-dashboard-auth.md` — the immediately preceding decision this refines.
- `project_auth_decision.md` memory — current state (updated alongside this ADR).
- `internal/session/`, `internal/platform/migrate/sql/0066_dashboard_sessions.up.sql`, `web-v2/src/contexts/AuthContext.tsx`, `web-v2/src/lib/auth.ts` — the implementation surface.
