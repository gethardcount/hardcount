# ADR-019: Stripe re-connect flushes stuck tax invoices

**Status:** Accepted
**Date:** 2026-05-04

## Summary

When an operator connects Stripe (`POST /v1/settings/stripe`), Velox retries tax on every draft invoice stuck on `provider_not_configured` or `provider_auth` in that mode, because connecting means the credentials are fixed. The response returns the count as `retries_queued`. A background goroutine with a 5-minute limit runs each retry through `Service.RetryTax`, which also finalizes invoices the engine made ([ADR-017](017-tax-retry-and-auto-finalize.md)). There is no durable queue: if the process stops mid-run, connecting again or a per-invoice Retry tax finishes the job. Status: active.

## Context

Two earlier ADRs frame this:

- **ADR-017** (background tax-retry reconciler) auto-retries
  invoices stuck on transient codes (`provider_outage`, `unknown`)
  via an exponential backoff schedule. It deliberately **excludes**
  operator-action codes (`provider_auth`,
  `provider_not_configured`, `customer_data_invalid`,
  `jurisdiction_unsupported`) — auto-retrying without operator
  acknowledgement would burn provider quota on permanently-failing
  invoices.

- **ADR-018** added a Retry advance button so operators recover
  from a stuck test-clock catchup without delete-and-rebuild.

Between those, one gap remained: when an operator (re)connects
Stripe in Settings → Payments — the explicit signal "I just fixed
the credentials" — invoices that were stuck on
`provider_not_configured` or `provider_auth` for that
(tenant, livemode) had no automatic recovery. The operator had to
click Retry tax on every stuck invoice individually. Discovered
2026-05-04 by an operator who connected test-mode Stripe and then
asked "should retry tax happen automatically now or wait?" —
answer was "neither, you have to click each."

Industry parallel: Stripe Connect replays queued events when an
account reconnects. Lago / Recurly do the equivalent — the
re-connect IS the operator signal, so the system should catch up
on its own.

## Decision

When `tenantstripe.Service.Connect` succeeds (verify returns 200
from Stripe), the service:

1. Synchronously counts invoices for that (tenantID, livemode)
   matching `status='draft' AND tax_status IN ('pending', 'failed')
   AND tax_error_code IN ('provider_not_configured',
   'provider_auth')`. The count is included in the Connect HTTP
   response as `retries_queued` so the dashboard renders
   "Retrying N stuck invoices in the background".
2. Fans out a goroutine to actually run the retries. The
   goroutine builds a fresh `context.Background() +
   WithTenantID + WithLivemode + 5-min timeout` (per the
   ctx-attribute audit memory: every ctx-detached worker pins all
   relevant attrs, not just livemode). Calls
   `invoice.Service.RetryProviderConfigErrors`, which iterates
   the stuck rows and invokes existing `Service.RetryTax` per
   row. RetryTax already includes the auto-finalize chain from
   ADR-017 — so engine-generated invoices that recompute clean
   move from `draft` → `finalized` in the same goroutine pass.

The retry path goes through the existing single source of truth
(`Service.RetryTax`); this ADR is purely a new trigger, no new
recompute logic.

### Scope of the retry

Only `provider_not_configured` and `provider_auth` — both Stripe-
configuration codes that fresh credentials directly resolve.
`customer_data_invalid` and `jurisdiction_unsupported` need
different operator actions (edit billing profile / register with
Stripe Tax) and don't belong on the Connect path.

### No retry-attempt cap

The 8-attempt cap on the background reconciler exists to bound
provider-quota burn on permanently-transient errors. Connect-
triggered retry is a one-shot human signal — if the new
credentials still don't work, the per-invoice retry surfaces a
fresh error code and the operator sees that immediately. A cap
would be redundant.

### Sync vs async

Async goroutine, returning fast from Connect. A synchronous fan-
out would block the Connect HTTP response on potentially many
sequential Stripe Tax round-trips (~200ms each); 100 stuck
invoices × 200ms = 20-second spinner. Bad UX. The 5-min wall-
clock timeout on the goroutine bounds resource use; in-flight
retries past that exit cleanly and remaining rows can still be
recovered by an operator-driven Retry tax click per invoice.

### No durable retry queue

If the process dies mid-fan-out, in-flight retries are lost. Two
recovery paths exist without durable persistence:

1. The operator re-clicks Connect (idempotent — fan-out re-runs
   against any rows still stuck).
2. The operator clicks Retry tax on individual invoices.

Adding a durable queue would be belt-and-suspenders (per the
relevant memory) for a low-frequency, operator-initiated event.

## Consequences

### Positive

- Operator's mental model matches: "I fixed the underlying issue
  → the system catches up." No per-invoice clicking.
- Frontend gets immediate feedback ("Retrying 3 stuck invoices in
  the background.") so the operator knows recovery is in flight.
- Single source of truth: retry logic stays in `Service.RetryTax`.
  This ADR is a trigger, not a parallel implementation.
- The auto-finalize chain (ADR-017) means engine invoices flip
  from draft → finalized as part of the same fan-out — no second
  pass needed.

### Negative

- A flaky network during the goroutine's run leaves some rows
  stuck. Acceptable: re-Connect fixes it; Retry tax per-invoice
  also fixes it.
- Connect now has a per-tenant query (`COUNT`) before responding.
  Sub-millisecond on the indexed predicate; negligible.
- Race vs operator manual click: both can target the same row.
  `UpdateTaxAtomic` locks `FOR UPDATE`, so one wins; the loser
  gets a 409 and aborts that one row. Net: each row resolves
  exactly once.

## Compatibility

- API: `POST /v1/settings/stripe` response shape gains a
  `retries_queued` integer field. Existing clients that ignore
  unknown fields work unchanged.
- Frontend: Settings → Payments connect toast renders "Retrying
  N stuck invoices in the background." when N > 0.
- No schema change.
- Backwards-compatible with deployed binaries — the field is
  additive on the response and the goroutine is gated by
  `SetStuckRetrier` being wired (no-op if not).

## Notes

- `tenantstripe.Service.SetStuckRetrier` is the production
  wiring; the no-op fallback when nil keeps narrow unit tests
  free of cross-package imports.
- `invoice.Service.CountProviderConfigErrors` exists alongside
  `RetryProviderConfigErrors` so Connect can populate the
  response without waiting for the goroutine. Both back to the
  same `Store.ListProviderConfigErrors` query — count is just
  `len(rows)`.
- The 5-min `PostConnectRetryTimeout` covers ~1500 invoices at
  Stripe Tax's typical ~200ms/call latency. Far beyond expected
  per-tenant volumes.
