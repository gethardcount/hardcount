# ADR-017: Background tax-retry reconciler + auto-finalize on success

**Status:** Accepted
**Date:** 2026-05-04

## Summary

When tax calculation fails, the invoice stays a draft and a background job retries the tax. It waits 5 min, 15 min, 1 h, 4 h, 12 h, 1 day, 2 days and 4 days (±10% jitter), then stops after 8 attempts and leaves the error showing. Only `provider_outage`, `unknown` and, since 2026-07-29, `provider_not_configured` are retried; the other codes need an operator action. When a retry succeeds on an invoice the engine made (`billing_reason` is not `manual`), the invoice is finalized automatically, and manual drafts stay drafts. Status: active.

## Context

When tax calculation fails during invoice finalize, the engine
defers: `tax_status='pending'`, `tax_pending_reason` carries the
provider error, the invoice stays in draft. Migration 0039
(*"block-and-retry tax calculation"*) laid the schema for this
flow and explicitly documented:

> A background worker retries.

The worker was never built. Stuck invoices waited indefinitely on
an operator click.

Two latent problems followed:

1. **No automatic retry of transient failures.** Stripe Tax 5xx /
   timeout / connection-refused errors are recoverable on the next
   call; without a worker, every transient blip required manual
   action.
2. **Two-click recovery for the eventually-resolved case.** Even
   when an operator clicked Retry tax and it succeeded, the
   invoice stayed draft until they clicked Finalize again. The
   Retry succeeded → Finalize never happened → the invoice never
   got issued, sometimes for hours.

The problem surfaced on 2026-05-04 when an operator's draft
invoice (DEMO-000004) sat in `tax_status='pending'` for hours
because the test-clock catchup had hit a context-bug
("no client configured for livemode=false") that was already
fixed; the row was never re-attempted.

## Decision

Two coupled changes:

### 1. Background tax-retry reconciler

The billing scheduler grows a tax-retry tick (right after the
payment reconciler, before charge retries). Per tick:

```
ListPendingTaxRetry(retryable_codes, max_attempts) →
  for each invoice:
    Engine.RetryTaxForInvoice(...) →
      ApplyTaxToLineItems(...) → atomic UpdateTaxAtomic(...)
    Service auto-finalizes on success (next subsection)
```

**Retry scope.** Only invoices with
`tax_error_code IN ('provider_outage', 'unknown')` are retried.
Other typed codes are operator-action-required by their nature:

| Code                     | Retried? | Rationale                                               |
| ------------------------ | -------- | ------------------------------------------------------- |
| `provider_outage`        | Yes      | Stripe 5xx / timeout — transient by definition          |
| `unknown`                | Yes      | Classifier didn't infer; budget covers it cheaply       |
| `provider_auth`          | No       | Operator must rotate the key                            |
| `provider_not_configured`| No       | Operator must connect Stripe in Settings → Payments     |
| `customer_data_invalid`  | No       | Postal code / country won't appear from a retry         |
| `jurisdiction_unsupported` | No     | Tenant must register or override                        |

**Backoff curve.** Eight attempts spread over ~10 days:

```
1st retry → +5  min     5th → +12 hours
2nd       → +15 min     6th → +1  day
3rd       → +1  hour    7th → +2  days
4th       → +4  hours   8th → +4  days
```

±10% jitter per interval to avoid thundering herd when a Stripe
Tax outage recovers and many stuck invoices retry within the
same second.

References walked: Stripe Smart Retries (8 attempts, ~3 weeks),
Recurly's tax cron (5min → 30min → 1h → 6h → 24h, max ~10),
Chargebee (transient-only), Lago (no automatic retry).

**Cap.** After the 8th attempt the row is no longer fetched
(`tax_retry_count < maxAttempts` filter). It stays at its last
`tax_error_code` so the operator-facing attention surface remains
live; the worker just stops burning provider quota.

**Schema.** Migration 0074 adds `invoices.tax_next_retry_at
TIMESTAMPTZ` plus a partial index narrowed to draft invoices in
`tax_status IN (pending, failed)` so the scheduler scan is cheap
even after the table grows.

### 2. Auto-finalize on successful retry

When `Service.RetryTax` (operator-driven OR scheduler-driven)
sees a successful tax recompute (`tax_status='ok'`), it chains
into the existing `Service.Finalize` path **iff** the invoice is
engine-generated (`billing_reason != 'manual'`).

The auto-finalize gate is simple but deliberate:

```go
inv.Status == InvoiceDraft &&
inv.TaxStatus == InvoiceTaxOK &&
billing_reason ∉ {"", "manual"}
```

- **Engine invoices** (subscription_cycle, threshold, proration)
  finalize automatically. The pending state was an anomaly, not
  a deliberate review checkpoint; the operator wants the
  customer to receive the invoice as soon as tax resolves.
- **Manual drafts** stay draft. Operator may still be building
  the invoice — adding line items, editing memo. Auto-finalize
  would surprise.

`Finalize` failure post-retry doesn't unwind the tax recompute —
the invoice stays draft with the new tax decision and a logged
warning so post-mortems can answer "why didn't it auto-finalize?"

References: Recurly + Chargebee both auto-finalize subscription
invoices on tax-success. Stripe + Lago require a manual finalize
step but their architectures decouple finalize from tax in a way
Velox doesn't.

## Consequences

### Positive

- Transient Stripe Tax failures (provider's 5xx) self-heal
  without operator action. The dashboard banner clears as soon as
  the worker succeeds.
- Operator-triggered "Retry tax" becomes one click for engine
  invoices instead of two.
- The reconciler is tightly scoped to retryable codes — no
  silent quota burn on permanently-failing invoices.
- Existing Service.Finalize is the single source of truth for
  state transition + public-token + tax-commit; auto-finalize
  reuses it instead of duplicating.

### Negative

- After the 8-attempt cap, an invoice with persistent
  `provider_outage` won't retry automatically. Operator must
  intervene. Mitigation: 8 attempts over ~10 days is enough to
  cover any realistic transient outage; longer than that *is*
  operator-action territory.
- The reconciler crosses tenants. RLS-bypassed scan is correct
  here (same shape as test-clock sweeper, payment reconciler),
  but tenant-scoped audit log entries from auto-finalize need to
  identify the worker as the actor, not a user.

## Compatibility

- API surface unchanged. `POST /v1/invoices/{id}/retry-tax` still
  returns 200 with the updated invoice. The new behavior: when
  the retry succeeded AND the invoice is engine-generated, the
  returned invoice's `status` is now `finalized` instead of
  `draft`.
- Operators currently dependent on the two-click flow (manual
  invoices) see no change — gate excludes those.
- `tax_next_retry_at` is a new field on `Invoice` JSON; client
  code that doesn't know about it ignores cleanly.

## Notes

- `taxRetryableCodes()` returns a function-built slice (not a
  package-level slice constant) so callers always get a fresh
  copy and can't accidentally mutate the global.
- The existing `tax_retry_count` column is incremented per
  attempt by `UpdateTaxAtomic`; the new code reads it to gate
  the cap. No double-counting between operator-driven and
  scheduler-driven retries — both go through the same atomic
  update.
- The reconciler runs leader-gated via the existing billing
  advisory lock (no new lock key required) since it's part of
  `runBillingHalf`.
