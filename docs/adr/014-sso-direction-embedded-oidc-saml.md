# ADR-014: SSO Direction — Embedded OIDC/SAML, No SaaS Auth Vendor

**Date:** 2026-05-02
**Status:** Accepted. Corrected 2026-10-18: `zitadel/saml` serves the
identity-provider side, so it is withdrawn and the SAML library is
chosen when SSO is built; see the
[amendment](#amendment-2026-10-18-the-saml-library-serves-the-other-side).

## Status
Accepted

## Date
2026-05-02

## Summary

Not built yet: this ADR records the direction for when a design partner asks for SSO. Velox will embed OIDC and SAML client libraries in the binary and point each tenant at its own identity provider. The OIDC library is `github.com/zitadel/oidc/v3`. Corrected 2026-10-18: the SAML library named below, `github.com/zitadel/saml`, implements an identity provider, not the service provider Velox would be, so the SAML library is chosen when SSO is built. It will not use a SaaS auth vendor (WorkOS, Auth0, Clerk) or require a separate identity server, because self-hosted and air-gapped installs must work without outside calls. Password login stays homegrown ([ADR-011](011-email-password-auth-and-clean-api-keys.md)), team members have no enforced roles today ([ADR-081](081-minimal-team-invites.md)), and the MFA design is [ADR-095](095-login-security-roadmap-and-mfa.md). Status: active as a direction.

## Context

ADR-011 ships homegrown email+password for the dashboard and Bearer keys
for the API, with the deferred question: "what shape does SSO take when
a design partner asks for it?" Earlier memory and the README left this
loose with phrasing like *"WorkOS / Zitadel deferred until a design
partner asks"*, which has caused recurring confusion in planning
sessions ("are we using WorkOS?" → no, but the doc reads ambiguous).

This ADR closes that loop with a concrete direction so the next time
SSO comes up, the path is predetermined and one less decision under
DP-deadline pressure.

### What the OSS landscape actually does (researched 2026-05-02)

Auth stacks across peer self-hostable OSS dev tools:

| Project | Default OSS auth | SSO path |
|---|---|---|
| Cal.com | NextAuth.js + native creds | BoxyHQ Jackson (in-process SAML/OIDC) |
| n8n | Homegrown bcrypt + roles | Built-in SAML (paid) |
| Plausible CE | Homegrown bcrypt | Community-requested, not shipped |
| PostHog | Django auth | OAuth + multi-tenant SAML in-process |
| Supabase | `supabase/auth` (GoTrue fork, embedded Go service) | OIDC/SAML in-process |
| Metabase | Homegrown + Google OIDC + LDAP | SAML/JWT paywalled (Pro/Enterprise) |
| Outline | None — external IdP required | Customer's Keycloak/Authentik/etc. |
| Penpot | Homegrown + LDAP + generic OIDC | OIDC built-in |
| **Lago (OSS billing)** | Homegrown bcrypt + JWT | Okta paywalled as Enterprise add-on |
| **KillBill (OSS billing)** | Apache Shiro embedded; RBAC in OSS | LDAP/external IdP via Shiro |

The dominant pattern: **homegrown password auth for the OSS tier; SSO
delivered as in-process OIDC/SAML libraries that talk to whatever IdP
the customer already runs**. WorkOS / Auth0 / Clerk / Stytch all
require an account at their SaaS — disqualifying for any product whose
positioning is "runs entirely in your VPC."

### Velox-specific constraints

- **Self-host is a core wedge.** Velox's positioning is "the open-source
  billing engine for AI and usage-heavy SaaS — runs in your own VPC."
  Any auth path that requires reaching workos.com / auth0.com /
  clerk.com at runtime breaks that promise. Air-gapped customers exist
  in the target market (regulated tenants, EU GDPR-strict, India RBI
  data localization).
- **Single binary deployment is a feature.** Velox is one Go binary +
  Postgres. Forcing customers to also deploy and operate Keycloak or
  Authentik as a precondition for login would be a regression.
- **Customer's IdP is variable.** A DP might run Okta, Entra (Azure
  AD), Google Workspace, Keycloak, Authentik, JumpCloud, or
  PingIdentity. Velox can't pick one vendor. Standards-based OIDC and
  SAML are the only thing every IdP speaks.

## Decision

When SSO becomes a real DP requirement (the trigger named in ADR-011 +
`project_auth_decision`), Velox will:

1. **Stay homegrown for the password path.** Do not migrate
   `internal/user` to a third-party identity service. The bcrypt +
   sessions recipe in ADR-011 is the OSS default and stays.
2. **Embed OIDC and SAML libraries in-process** for SSO, using
   `github.com/zitadel/oidc/v3` and `github.com/zitadel/saml`. Apache-2.0
   licensed Go libraries from a credible upstream (Zitadel — modern,
   actively maintained), shipped as imports, no separate Zitadel
   server to deploy.
3. **Wire SSO via per-tenant config**: `tenant_settings.sso_oidc_issuer`,
   `tenant_settings.sso_saml_metadata_url`, etc. The tenant points
   Velox at their IdP; Velox initiates the OIDC/SAML flow at runtime
   inside its own process. No SaaS dependency, no Velox-owned IdP.
4. **RBAC stays homegrown.** Roles + permissions in `users.role` /
   `user_tenants.role` (already partially built per ADR-011),
   middleware-enforced. KillBill is the model — embedded RBAC in OSS,
   not a paid add-on. Whether RBAC ever becomes a paywall is a
   pricing-and-packaging decision separate from the auth-vendor
   decision and out of scope here.

### What is rejected

- **WorkOS / Auth0 / Clerk / Stytch as the OSS default.** SaaS-only
  deployment models break the self-host wedge. Disqualifying.
- **External Keycloak / Authentik as a hard requirement.** Adds an
  operational burden no DP has asked for and breaks the single-binary
  story. Optional integration via OIDC is fine — that's just "your
  Keycloak is the IdP, Velox is the OIDC client" and falls out of
  decision #2 for free.
- **Paywalling SSO as the enterprise upsell, Lago-style.** This ADR
  doesn't decide pricing; it decides architecture. SSO ships in OSS by
  default because once the OIDC/SAML libs are imported, gating them
  costs more than not gating them. Paywall decisions, if they ever
  happen, gate higher-tier features (RBAC fine-grain, audit-log
  retention, etc.) rather than the protocol surface.
- **WorkOS as a hosted-Velox-only optional integration.** Could be
  reconsidered if hosted Velox ever exists; not on the OSS tier path.

## Consequences

### Positive

- **Air-gap safe.** No outbound runtime calls for auth.
- **Single binary stays single binary.** The OIDC/SAML libs add
  ~200–400 lines of integration code, not an extra service.
- **IdP-agnostic.** Any standards-compliant IdP works on day one —
  Okta, Entra, Google Workspace, Keycloak, Authentik, JumpCloud,
  PingIdentity, Rippling, OneLogin. Beats Lago's "we charge for the
  Okta connector" framing.
- **No vendor lock-in.** Zitadel libs are Apache-2.0 Go. If Zitadel as
  a project disappears tomorrow, the libraries are still in our `go.sum`
  and forkable; OIDC/SAML are open standards regardless.
- **Closes the WorkOS-confusion loop.** Future planning sessions can
  point at this ADR instead of re-litigating the vendor question.

### Negative

- **More integration work than buying.** ~200–400 lines vs. zero with a
  managed AuthKit. Acceptable: the integration is a one-time cost, the
  vendor dependency would be permanent.
- **No flashy hosted login UI out of the box.** Have to render our own
  login page (already done in `web-v2/src/pages/Login.tsx`) and our
  own `/auth/sso/initiate` redirect flow. Standard work.
- **Self-hosted ops own the IdP.** A customer who doesn't run an IdP
  can still use email+password — unchanged from today. SSO is purely
  additive. Customers who want SSO bring their own IdP, which is
  exactly the assumption every IdP-agnostic enterprise tool makes
  (and what they expect anyway).

## Trigger to revisit

- A design partner explicitly demands SSO / SAML / MFA.
- A second human on a tenant needs named-actor audit attribution
  (multi-user RBAC).

## References

- ADR-011 — email+password dashboard auth, ground truth for the OSS path.
- `feedback_pre_launch_scoping` memory — defer features without named
  customer demand; this ADR is the design predetermined for when that
  demand arrives.
- `project_auth_decision` memory — kept aligned with this ADR.
- `feedback_amend_decisions_when_course_changes` memory — the WorkOS
  ambiguity in the previous version of `project_auth_decision` is
  exactly the silent-deviation pattern this ADR closes.

## Amendment 2026-10-18: the SAML library serves the other side

Decision 2 names `github.com/zitadel/saml`. That library implements a SAML identity provider: its README calls it "a server implementation" of SAML, and its feature table covers only the identity provider. In this design Velox is the service provider. The tenant's identity provider (Okta, Entra and so on) signs an assertion, and Velox checks it and signs the user in. `zitadel/saml` does not do that, so it is withdrawn.

The SAML library is chosen when SSO is built, by testing each candidate against the identity providers a design partner names. Service-provider libraries to test first:

- `github.com/russellhaering/gosaml2` (Apache-2.0; v0.12.0, 2026-08-05), for service providers only.
- `github.com/crewjam/saml` (BSD-2-Clause; v0.5.1), whose `samlsp` package is a service provider.

The OIDC choice stands: `zitadel/oidc/v3` has a relying-party (client) package, `pkg/client/rp`. `github.com/coreos/go-oidc/v3` (Apache-2.0) is the smaller client to compare it with at build time. The rest of the decision is unchanged. Found by the build-vs-library audit of 2026-10-17.
