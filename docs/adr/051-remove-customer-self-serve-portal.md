# ADR-051: Remove the customer self-serve portal

- Status: Accepted
- Date: 2026-06-09
- Relates: ADR-039 (coupons cut — same retain-schema pattern), the B2B-not-B2B2C auth stance, the positioning wedge (AI-native, self-host)

## Summary

The customer self-serve portal (magic-link login and the `/v1/me/*` API) was removed. Self-serve billing portals are a consumer and small-business pattern, while Velox's B2B users build their own billing UI and route lifecycle changes through the operator. Customers still add cards on a Stripe-hosted setup page, pay through the hosted invoice link and see usage through the cost-dashboard token. The portal session and magic-link tables are kept in case a future customer group needs a portal. Status: active.

## Context

Velox shipped a customer **self-serve portal**: a magic-link login (`/portal`) where an operator's end-customer authenticates via an operator-issued session (`vlx_cps_…` / magic-link `vlx_cpml_…`) and self-manages their subscription — view/pay invoices, cancel/resume, manage payment methods, edit profile. Backend: `internal/portalapi` (the `/v1/me/*` handler) + `internal/customerportal` (sessions + magic-links). Frontend: `Portal.tsx` / `PortalLogin.tsx` / `PortalMagic.tsx` + `portalAuth.ts`. ~3,000 LOC.

A spine/wedge audit flagged it as off-wedge B2C-flavored surface maintained for zero design partners. Before cutting working customer-facing code, we did multi-platform industry research (Stripe, Chargebee, Recurly, Zuora + AI-native Orb/Metronome/Lago + real AI infra: OpenAI, Anthropic, Modal, Replicate, Baseten).

## Findings (source-quoted)

- **A hosted self-serve portal is fundamentally a B2C/SMB/PLG pattern.** Stripe's Customer Portal is "a secure, Stripe-hosted page" for SMB self-service; for usage-based subscriptions it "can only be canceled, not modified."
- **AI-native peers do NOT ship a lifecycle portal.** Orb/Metronome/Lago emphasize *embeddable* in-product cost/usage surfaces the vendor builds on their APIs; Lago's portal is read-only. Metronome: build dashboards "directly off of the Metronome API."
- **Every real AI infra/API company builds its own billing UI** in its own console (platform.openai.com, console.anthropic, Modal/Replicate/Baseten settings). None redirects customers to a billing-vendor's hosted portal. Self-serve there = top-up credits / set spend limits / watch usage — not "log in to a portal to cancel."
- **B2B billing is operator-/contract-mediated**: AP pays via wire/ACH, lifecycle changes are CSM-negotiated, not a portal button.

For Velox's stated first design partner (B2B AI infra), the customer surface that matters is the **embeddable cost dashboard** (a wedge build-list item) — not a subscription self-service portal.

## Decision

**Remove the customer self-serve portal.** Delete `internal/portalapi`, `internal/customerportal` (portal sessions + magic-links), the three frontend portal pages + `portalAuth.ts`, the `/v1/me/*` mounts, the `/v1/public/customer-portal` + `/v1/customer-portal-sessions` routes, the portal request adapters, the idempotency customer-scoping branch, and the magic-link email path.

This reverses the freeze-don't-cut call recorded earlier the same day: the research showed the portal is the B2C pattern, not the B2B/AI-infra norm, so it's not a working feature worth keeping for this ICP — it's off-wedge surface for no audience.

## What survives (verified — charging is NOT affected)

- **Card collection** — fully operator-driven and independent of the portal: `/v1/customers/{id}/payment-methods` (operator API-key) with **send-setup-email** / **copy-link setup-session** → the customer enters their card on a **Stripe-hosted Checkout setup page** (Modal-style), `setup_intent.succeeded` saves it. `paymentmethods.Service` (load-bearing for the charge path) stays; only its customer-facing `/v1/me/payment-methods` routes were removed.
- **Payment** — customers still pay via the **hosted-invoice public token** (separate surface).
- **Cost/usage visibility** — the **cost-dashboard public token** (separate surface) — the real AI-native customer surface.
- **Lifecycle** — cancel/resume via the **operator dashboard** (the B2B norm).
- **Email blind index** — retained (it also powers bounce reporting + recipient suppression, not just magic-link lookup).

## Retain, don't drop

The `customer_portal_sessions` and `customer_portal_magic_links` **tables are retained** (same pattern as ADR-039 coupons): rebuild is a thin handler over surviving services if a future ICP (B2C/SMB self-serve SaaS) needs it. No DROP migration.

## Consequences

- ~3,000 LOC removed; cleaner B2B positioning; the codebase no longer carries a customer-facing surface for an audience that doesn't exist yet.
- Customer auth surfaces are now: **hosted-invoice token + cost-dashboard token** only (portal-session + magic-link removed). Amends the B2B-not-B2B2C surface list.
- Rebuild trigger: a design partner whose end-customers self-manage billing (a B2C/SMB ICP shift). Until then, operator + hosted-invoice + cost-dashboard cover the B2B flows.
