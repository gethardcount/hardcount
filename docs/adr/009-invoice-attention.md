# ADR-009: Unified Invoice Attention Surface

**Date:** 2026-04-30
**Status:** Accepted

## Status
Accepted

## Date
2026-04-30

## Summary

Every invoice in an API response carries one `attention` field that says whether the operator needs to act, and why. It holds a severity, a typed `reason`, an open dotted `code`, a message safe to show, and a short list of prescribed `actions`. The server computes it from the invoice's stored fields on each read, never stores it, and omits it when there is nothing to act on. The reason list has grown since: the current set is in `internal/domain/invoice_attention.go` (`overdue` is no longer one), and `doc_url` stays empty until error pages are published. Status: active.

## Context

When an invoice gets stuck — tax calculation failed, payment declined, payment outcome ambiguous, past due — the operator's first question is *"why, and what do I do about it?"*. Until now, Velox surfaced these signals piecemeal:

- `tax_status` + free-form `tax_pending_reason` for tax deferrals
- `payment_status='failed'` + `last_payment_error` for payment declines
- `payment_status='unknown'` for ambiguous Stripe outcomes
- `payment_overdue` boolean for past-due lifecycle
- An `OperatorContextCard` component on `InvoiceDetail.tsx` that hand-coded a free-text diagnosis line per cause

The dashboard had to know which fields meant what, and the per-cause copy and CTAs lived inside the React component. SDK consumers had to re-implement the same reduction. There was no single signal an operator could filter on ("show me everything that needs attention"), no typed reason taxonomy webhook consumers could route on, and no machine-readable affordance for "what to do next".

A 2026-04-30 industry survey across Stripe (`last_finalization_error` / `last_payment_error` typed envelopes, per-cause `invoice.*` events), Chargebee (`dunning_status`), Recurly (lifecycle states with prescribed action verbs), Lago (`payment_status` + per-cause webhooks), Maxio, and Zuora found a consistent pattern at the four mature platforms (Stripe, Chargebee, Recurly, Maxio):

1. A typed reason / state code on the resource
2. A human message safe to render verbatim
3. A `doc_url` deep-link to "how to fix this" docs
4. 2–4 prescribed action verbs (Retry / Edit / Mark Paid / Void / Stop Dunning)
5. Per-cause webhook events (no aggregate "attention_changed")

The two platforms that diverged (Lago, Zuora) underinvest in operator UX — Lago's dashboard is thinner because they expect external tooling; Zuora hands the operator a `BillRun` job result and lets enterprise tooling teams build the resolution flows. Velox's positioning ("AI-native billing engine you can self-host") matches the first cluster.

## Decision

Ship a unified `attention` field on every Invoice payload, computed server-side from the durable fields on each read.

Wire shape:

```json
"attention": {
  "severity": "critical",
  "reason": "tax_location_required",
  "message": "The customer's billing profile has missing or invalid data the tax provider requires.",
  "actions": [
    { "code": "edit_billing_profile", "label": "Edit billing profile" },
    { "code": "retry_tax", "label": "Retry tax" }
  ],
  "code": "tax.customer_data_invalid",
  "doc_url": "https://docs.velox.dev/errors/tax-location-required",
  "param": "customer.address.postal_code",
  "detail": "stripe tax: {\"message\":\"...\",\"code\":\"customer_tax_location_invalid\"}",
  "since": "2026-04-30T18:22:11Z"
}
```

Healthy invoices return no `attention` key (omitempty). Terminal-state invoices (paid, voided) suppress attention even when underlying signals would otherwise trigger it.

### Field rationale

| Field | Source | Rationale |
|---|---|---|
| `severity` | `info` / `warning` / `critical` closed enum | Drives chip colour, queue ordering. Stripe doesn't ship this; we do because it's cheap and design partners notice. |
| `reason` | Closed enum (5 codes today) | What the dashboard switches on for icon/copy/CTA layout. Stable contract — codes never repurposed. |
| `code` | Open dotted string | Programmatic clients route on this. Stripe parity (`code` is open). New codes ship without a major version bump. |
| `message` | Free text | Human-readable headline. Safe to render verbatim. |
| `actions` | Closed enum codes + labels | Prescribed CTAs. Closed because every code maps to a server endpoint or frontend route, and audit logs key off the code. |
| `doc_url` | Computed `docs.velox.dev/errors/<reason>` | Stripe parity. Required so operators get a "Learn more" path, not silence. |
| `param` | Dotted-path field pointer | Stripe parity. Lets the dashboard's Edit CTA deep-link to the broken field. |
| `detail` | Raw provider payload | Stripe Tax JSON envelope, last payment error message. Disclosed in collapsible section. |
| `since` | `tax_deferred_at` / `due_at` / `updated_at` | Operators triage by age. |

### Reason taxonomy — five codes today

```
tax_calculation_failed   — generic tax computation failure (jurisdiction, outage, auth, unknown)
tax_location_required    — specific case: customer address missing/invalid
payment_failed           — provider declined the charge
payment_unconfirmed      — Stripe returned an ambiguous outcome (5xx/timeout)
overdue                  — past due_at, still unpaid
```

Reserved-but-not-yet-emitted (planned, code reserved): `finalization_failed`, `payment_action_required`, `payment_method_required`, `dunning_exhausted`, `dispute_lost`. Adding these later is a non-breaking extension — the enum is documented as open-for-extension in the OpenAPI spec.

### Action codes — closed enum

```
edit_billing_profile, retry_tax, retry_payment, wait_provider,
rotate_api_key, reconcile_payment, review_registration
```

Mirrors the Stripe / Recurly / Chargebee verb cluster identified in the research. Closed because the audit log persists action codes verbatim.

### Implementation

- `internal/domain/invoice_attention.go` — types + `ClassifyInvoiceAttention(Invoice) *Attention`. Pure function, no I/O, deterministic on input fields.
- `internal/invoice/service.go` — `attachAttention()` helper called by `Get`, `List`, `GetWithLineItems`, `GetByPublicToken`, `ApplyCoupon`, `RetryTax`. Internal callers (engine, scheduler) reading directly from `Store` skip it.
- `internal/billing/engine.go` — `RetryTaxForInvoice(ctx, tenantID, invoiceID)` re-runs `ApplyTaxToLineItems` and persists atomically via `UpdateTaxAtomic`. Backs the `retry_tax` action.
- `POST /v1/invoices/{id}/retry-tax` — operator-triggered tax recompute. Audit-logged with before/after attention reason.
- `migration 0067` adds `tax_error_code` column with CHECK constraint; the engine deferral path populates it via `tax.Classify(err)`.

### What we deliberately did NOT do

**Persist `attention` as a column.** Tempting (would enable SQL-level filtering "WHERE attention.reason = 'tax_failed'"), but creates a stale-attention class of bug — every state transition that touches a source field would have to also rewrite attention or risk drift. Derive-on-read is the safer default; we add a persisted projection only when SQL list filtering becomes load-bearing.

**Aggregate `invoice.attention_changed` webhook event.** Zero precedent across the six platforms researched. Per-cause events (`invoice.tax_failed`, `invoice.payment_failed`, etc.) match Stripe and let consumers subscribe selectively. Webhook event work is deferred to a follow-up commit.

**Subscription-scoped attention (Maxio pattern).** Velox is invoice-scoped per the existing data model; the subscription page can summarize by aggregating its invoices' attention.

**Free-form action codes.** Audit logging requires stable codes; closed enum.

## Consequences

### Positive

- Single component (`InvoiceAttention.tsx`) renders the banner across detail, list, and (eventually) hosted-invoice pages.
- One typed contract for SDK consumers — they switch on `reason` for UX, route on `code` for alerting.
- Adding a new failure mode (e.g., `dispute_lost`) requires: enum entry on server, action codes if new verbs needed, doc page. No frontend changes for the long tail — the component renders any reason via the default-label map.
- Operators see prescriptive next steps inline (Stripe parity), not a free-text reason they have to interpret.

### Negative

- The classifier is centralized — every read path that wants attention populated has to go through `service.attachAttention`. Internal callers (billing engine, scheduler) reading the store directly don't get attention; this is intentional but easy to forget.
- Reason enum is a public contract — adding wrong codes early is expensive to walk back.
- Severity is an opinionated three-value enum; Stripe doesn't ship it and a future design partner could push back. The `code` field gives them an escape hatch (route on `code`, ignore `severity`).

### Open follow-ups

- Per-cause webhook events (deferred — research-recommended over aggregate).
- SQL-level "needs attention" filter on `GET /v1/invoices` — promote `attention.reason` to a persisted projection when list volume justifies it.
- Doc site pages at `docs.velox.dev/errors/<reason>` — currently linked but the URLs return 404.
- Background tax retry worker — today the only tax retry is operator-triggered. A worker that runs every N minutes against `tax_status='pending'` invoices closes the loop.
- Rate limiting on the `retry-tax` endpoint — `tax_retry_count` already exists; cap at e.g. 5 manual retries per invoice per hour once volume warrants.
