# ADR-007: Revert Dashboard to API-Key Auth

**Date:** 2026-04-29
**Status:** Superseded by ADR-011 (email/password dashboard auth shipped 2026-05-01)

## Status
Superseded by [ADR-011](./011-email-password-auth-and-clean-api-keys.md) — the API-key-as-dashboard-credential model was reversed back to email+password on 2026-04-30 once it became clear the cleanup costs (orphan-tenant safeguard, self-revoke 422, cookie fan-out, "current session" UI logic) were higher than the cost of a small homegrown auth surface.

## Date
2026-04-29

## Summary

Superseded by [ADR-011](011-email-password-auth-and-clean-api-keys.md) on 2026-05-01. This ADR made the dashboard log in with a pasted `vlx_secret_…` API key, kept in `localStorage` and sent as a Bearer header, and removed user accounts. The dashboard now signs in with email and password and an httpOnly session cookie, and API keys serve only SDK and curl callers. This file records why the API-key login was tried and then dropped.

## Context

Velox's `project_auth_decision.md` and `CLAUDE.md` always recorded the v1 auth plan as: *"API keys custom-built; user auth via WorkOS/Clerk later; no premature session/password infrastructure."* The intent was that the dashboard, like the public HTTP API, would be authenticated by the operator's API key — one shared secret per tenant, no user accounts in v1.

Despite that, an embedded email+password auth stack landed in the codebase: migration `0034_embedded_auth.up.sql` (users, user_tenants, sessions tables), migration `0035_member_invitations.up.sql`, and the `internal/dashauth`, `internal/session`, `internal/user`, `internal/dashmembers` packages. The dashboard frontend grew matching pages: Login, ForgotPassword, ResetPassword, AcceptInvite, Members. The decision document was never amended.

The deviation surfaced during a 2026-04-29 review when:
- The author asked for a brutal scope audit of features built ahead of evidence.
- Velox is local-only with one user account, zero design partners, zero contributors.
- A second class of memory entry (`feedback_amend_decisions_when_course_changes.md`) was added that explicitly names this anti-pattern.

The cost of carrying the home-grown stack forward is real: ~2k LOC of password hashing, session rotation, single-use token plumbing, timing-safe enumeration resistance, cookie security gating, member-invite flows, and the matching frontend. Every CVE in those primitives, every session-fixation discovery, every cookie misconfiguration is permanent maintenance load. Before any design partner has named the auth shape they need (Okta SSO? WorkOS? email+password? something else?), guessing is overhead.

## Decision

Revert the dashboard to API-key login. The operator pastes a `vlx_secret_…` key (printed by `make bootstrap` or visible on `/api-keys`) into a single-input login screen; the browser stores it in `localStorage` and rides every request as `Authorization: Bearer <key>`. No cookies, no sessions, no users, no invitations.

The backend exposes a tiny `GET /v1/whoami` that returns `{tenant_id, key_id, key_type, livemode}` from the validated bearer-key context. The dashboard hits it once on login to validate the pasted key and populate the `AuthContext`; it requires no specific permission so any valid key (publishable or secret) passes.

Migrations `0034` and `0035` stay in place. Down-migrations are destructive (and would lose any `users`/`user_tenants`/`sessions` rows the local dev database happens to hold), and the unused tables are cheap. Re-introducing user accounts later — whether email+password again, Zitadel, or WorkOS — does not require a fresh schema; it requires a fresh decision.

## Consequences

### Positive

- Realigns execution with the documented v1 plan in `project_auth_decision.md`.
- Removes ~2k LOC and four packages from ongoing maintenance: `internal/dashauth`, `internal/session`, `internal/user`, `internal/dashmembers`. Frontend loses ForgotPassword / ResetPassword / AcceptInvite / Members pages.
- Eliminates cookie-security configuration as a footgun (no more `Secure` flag gating, `SameSite` decisions, CSRF surface).
- Single auth path on `/v1/*`. The previous `session.MiddlewareOrAPIKey` adapter — which accepted either a session cookie or a bearer token, with a livemode-disagreement check between the two — collapses to plain `auth.Middleware`. Less branching in handlers.
- One container in Docker Compose (Postgres + Redis + Mailpit + the API) — no need to recommend a sibling identity provider to OSS self-hosters.

### Negative

- Login UX is "paste this opaque key" instead of "email + password." For developers evaluating an OSS billing engine this is normal (Stripe Dashboard, AWS, GitHub PATs all train this gesture); for non-technical operators it's worse. Velox has no non-technical operators yet.
- No per-human attribution in the audit log — every action is attributed to the API key, not a named user. The audit-log handler already supports this via `actor_type='api_key'`; nothing breaks, but multi-human tenants will want named actors.
- No member invitations. A second human on the same tenant either receives the secret key (poor) or waits for a future re-introduction of user accounts (correct).
- API key in `localStorage` is XSS-readable. The dashboard ships with strict CSP and no third-party scripts to mitigate; even so, this is a downgrade vs `httpOnly` session cookies. Acceptable at v0 dashboard scope; revisit when a tenant has data the XSS-of-localStorage threat actually targets.

### Trade-offs

- Trades user-account UX (and the security primitives that go with it) for ~weeks of recovered focus on the AI-native wedge (multi-dim meters, per-token billing primitives, model-tier cost views) — the actual product differentiator.
- Trades the half-built sunk-cost stack for a clean v0 surface that's straightforward to replace whole-cloth with whatever the first design partner names — Zitadel, WorkOS, custom email+password again, or something else.

## Trigger to revisit

Reintroduce user accounts when **any one** of:
- A design partner is in the room and explicitly demands real user accounts (multi-human tenants, named audit attribution, SSO, MFA, etc.).
- A tenant onboards whose security review will not accept "API key in browser localStorage."
- Velox has more than one human operator using the same tenant in dev.

When that trigger fires, evaluate Zitadel and WorkOS first — both give the surface (sessions, MFA, SAML, invites) for the cost of one additional service, vs the home-grown stack we just removed. Re-deriving the email+password stack from scratch is the option of last resort.

## Alternatives Considered

- **Stay with the in-house email+password stack.** Rejected. Compounds a documented deviation rather than correcting it; carries ~2k LOC of maintenance for an audience of one; doesn't replace the eventual SSO/MFA conversation that any real design partner will trigger anyway.
- **Migrate to Zitadel now.** Rejected for current stage. Adds a sibling container to the OSS self-host story for a feature no current customer needs. The first DP may want WorkOS instead, or specifically not Zitadel; building the integration before knowing optimises the wrong shape. The right time is when the trigger above fires.
- **Add a one-shot CLI command that mints a session cookie from a `vlx_secret_…` key.** Rejected. Introduces a sidecar UX (run a CLI to log in) without gaining anything over Bearer-on-every-request. The dashboard already has `Authorization: Bearer` plumbing; adding a session layer on top is a half-revert.

## Related

- `project_auth_decision.md` — original v1 plan (now updated to reflect the actual current state).
- `feedback_amend_decisions_when_course_changes.md` — meta-discipline this ADR enacts.
- `feedback_pre_launch_scoping.md` — sibling memory; same logic applied to other features cut on branch `lean-cut`.
- ADR-002 (per-domain package architecture) — `internal/auth/` remains the auth package; the four deleted packages were domain code, not platform.
