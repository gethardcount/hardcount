# ADR-089: Retire the audit fail-closed response swap (and the `audit_fail_closed` setting)

Date: 2026-07-13
Status: Accepted
Supersedes: the fail-closed half of the AuditLog middleware contract (no prior ADR covered it)

## Summary

A failed audit-log write never changes the API response: the handler's response is served as is, and the failure is logged and counted in `velox_audit_write_errors_total`. The per-tenant `audit_fail_closed` setting, which swapped a committed response for `503 audit_error`, is removed, and migration 0149 dropped its column. That 503 reported a failure for a change that had already committed, the idempotency layer cached it for 24 hours, and a retry with a new key could repeat the change. Writing audit rows inside the business transaction ([ADR-090](090-audit-in-tx-emission.md)) replaces fail-closed where it is in place, because the change and its row commit or roll back together. Status: active.

## Context

The AuditLog middleware records every successful mutating `/v1` request to the
append-only `audit_log`. On write failure, a per-tenant setting
(`tenant_settings.audit_fail_closed`) chose between fail-open (serve the
response, log + metric) and fail-closed (replace the handler's response with
`503 audit_error`, body says "retry").

The 2026-07-13 audit-subsystem e2e audit confirmed the fail-closed arm is not
just ineffective but actively money-unsafe, through an emergent interaction
with the Idempotency middleware (which mounts *outside* AuditLog):

1. **The 503 replaces a response whose mutation already committed.** The
   middleware runs after the handler; the business transaction (and any
   Stripe side effect) is already durable when the audit INSERT fails. The
   503 cannot un-commit anything — it is a lie about what happened.
2. **Idempotency caches the lie for 24 hours.** `finalizeKey` releases keys
   only for 409/422; every other status — including this 503 — is cached and
   replayed verbatim without re-entering the handler or AuditLog. A same-key
   retry can therefore *never* write the missing audit row nor retrieve the
   real response (`internal/api/middleware/idempotency.go`, releaseKey).
   Integrators' reconcilers see a permanent 503 for a charge that succeeded.
3. **Fresh-key and keyless retries double-mutate.** Idempotency keys are
   opt-in and DELETE is excluded from idempotency entirely, so the "paired
   with idempotency keys" safety claim in the old comment was false for
   exactly the callers who believed the body's "retry" instruction.
4. **A settings-lookup error forced fail-closed for everyone.** The
   `IsAuditFailClosed` lookup ran per-failure against the same database whose
   blip caused the audit failure; its error path defaulted to fail-closed, so
   during a DB blip effectively all tenants got 503s regardless of the
   setting.

Alternatives considered by the adversarial design panel:

- **Release the idempotency key on the audit 503** — rejected as a
  double-charge factory: the mutation committed, so a freed key means the
  retry re-executes it. Also requires the idempotency layer to special-case
  one status from one inner middleware (layering leak).
- **Keep the dial, fix the lookup** — rejected: once the swap is gone the
  dial has no subject. A setting that controls nothing is a lying knob.

## Decision

1. **Delete the response swap.** On audit write failure the middleware serves
   the handler's response untouched and records the failure
   (`velox_audit_write_errors_total{tenant_id}` + ERROR log). The middleware
   never mutates a response — banned as a class. The middleware this named has
   since been DELETED (ADR-090: the catch-all is gone, replaced by a route
   registry and a pure-observer detector), and so was its regression test. The
   invariant survived the replacement and is pinned on the detector that took its
   place: `TestAuditCoverage_NeverMutatesTheResponse` (plus
   `TestAuditCoverage_PreservesStreaming`) in
   internal/api/middleware/audit_detector_test.go.
2. **Retire `audit_fail_closed` end-to-end**: the middleware branch, the
   `AuditSettingsLookup` interface, the store's single-column lookup, and the
   `TenantSettings` field are removed. The DB column stays (harmless,
   defaulted) until the audit-redesign uninstall migration drops it; API
   callers that still send the JSON field get it ignored as an unknown key.
3. **Detach the explicit writer from caller cancellation.**
   `audit.Logger.Log` now uses `context.WithoutCancel` + 3s (mirroring the
   middleware's `writeAudit`), so a client disconnect after the business
   commit can no longer abort the richer audit row.

## What replaces fail-closed

Fail-closed semantics return **structurally** in the audit-emission redesign
(next ADR in this arc): audit rows are written in the same transaction as the
business mutation (`LogInTx`), so "mutation committed but audit row missing"
becomes unrepresentable — for every tenant, with no post-commit window to
police and no response swapping. Until that lands, all tenants run today's
default posture (fail-open + metric); no live tenant had opted in.

## Consequences

- The `audit_error` 503 body and its "retry" instruction no longer exist;
  clients can no longer be stranded behind a cached audit 503.
- `velox_audit_write_errors_total` is now unambiguous: every increment is a
  lost row on a served request (before, it conflated lost rows with blocked
  requests).
- MANUAL_TEST's settings-diff flow example changed (the fail-closed flip is
  gone); the flow's mechanics are unchanged.
- The interim window (until in-tx emission lands) accepts silently-metered
  row loss on audit-DB failure — exactly the pre-existing default posture,
  documented here so it is a decision, not an accident.
