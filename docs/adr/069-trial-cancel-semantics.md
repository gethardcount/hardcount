# ADR-069: Trial-cancel semantics — free at trial end, guarded in SQL at all four activation writers

**Date:** 2026-07-02
**Status:** Accepted (shipped in P2b-b; designed by the second P2b build-time panel — 3 lenses, SHIP-WITH-FIXES — which amended the plan's sketch; see plan §4.2)

## Summary

Cancelling a trial before it converts never bills, even when the job that activates trials runs late. Scheduling a cancel with `at_period_end` on a trialing subscription ends it at trial end with no invoice, and an immediate cancel of a trial that never activated is also free. An explicit `cancel_at` must be the trial end or on or after the end of the first paid period; a date in between returns 400. All four places that activate a trial check for a pending cancel inside their SQL `UPDATE`, so a cancel that commits during activation is never billed. Status: active.

## Context

Canceling a trial before it converts was broken three ways: the schedule
flag's meaning on a trialing sub was undefined (Velox's
`current_billing_period_end` while trialing is the end of the first **paid**
period — unlike Stripe, where it equals the trial end), `cancel_at` values
before period-end were rejected outright (making cancel-at-trial-end
inexpressible), and the **four** trial-activation writers — the wall-clock
scan, the clock-catchup scan, operator `EndTrial`, and the billing engine's
trial branch — would activate *and bill* a sub whose cancel committed
between their snapshot and their UPDATE. That last one is the original audit
bug, re-armed on a timer.

## Decisions

1. **`AtPeriodEnd` on a trialing sub means cancel FREE at trial end**
   (Stripe semantics). It stays a *flag* — never eagerly converted to a
   pinned `cancel_at` — so `ExtendTrial` moves it with the trial.
2. **`cancel_at` on a trialing sub has exactly two legal values**:
   `trial_end_at` (free cancel via the guard) or
   `≥ current_billing_period_end` (activate, bill period 1, cancel at its
   close). The open interval between them is a 400 naming both boundaries —
   no machinery honors a date inside it, while GET would have echoed it.
3. **Guards live in SQL, not just Go.** Both activation UPDATEs carry
   `AND cancel_at IS NULL AND cancel_at_period_end = false`; a blocked
   activation returns the typed `domain.ErrTrialCancelDue` and the caller
   routes. Go-level pre-checks remain as fast paths; the SQL predicate is
   the atomicity.
4. **`CancelAtTrialEnd` is a dedicated CAS transition**: trialing→canceled,
   `canceled_at = trial_end_at` (never the observing site's `now` — the
   engine path fires up to a full interval late), schedule fields cleared,
   **no invoice** (trials are free). The CAS predicates on the observed
   `trial_end_at` and a schedule actually due, so a `ClearScheduledCancel`
   or `ExtendTrial` committing in the gap *wins* — a customer who rescinded
   or was extended is never terminated off a stale snapshot
   (`domain.ErrTrialCancelConflict` = not handled this pass).
5. **`ScheduleCancellation` CAS-es on the status the intent was validated
   under.** The flag is status-polymorphic; landing a "free at trial end"
   intent on a just-activated sub would silently invert the promise. Drift →
   409, re-read.
6. **Winner fires once.** Terminal emissions — `subscription.canceled` with
   `canceled_by=schedule`, `reason=trial_end_cancel`, plus an
   `AuditActionCancel` row — fire only from the CAS winner, via one shared
   service helper (three service-side sites) and one engine helper.
   Deliberately **no `subscription.trial_ended`** (consumers read it as
   "billing begins" and would provision a canceled sub) and no invoice.
7. **The engine never bills on an activation conflict.** The pre-fix branch
   fell through to normal billing with its stale trialing snapshot on ANY
   activation error; once concurrent scans can *cancel*, that meant a full
   cycle-close invoice + advanced watermark on a terminal sub. Now it
   re-reads and bills only a genuinely-active row.
8. **Day-1 in_advance billing rides the activation transaction at every
   site.** The clock-catchup scan and the engine branch previously billed
   *after* the flip, WARNing the fee "will be deferred" — false: the cycle
   scheduler skips the just-elapsed segment, so the fee was simply lost.
   Both sites moved onto the `WithBill` coordinator-tx shape.
9. **`EndTrial` 409s on any pending schedule** ("clear the scheduled cancel
   first" — no silent precedence); **`ExtendTrial` 409s on a pending
   *explicit* `cancel_at`** (extending past a pinned timestamp strands it;
   nothing fires inside a running trial). Both enforced at service level for
   fast feedback AND in the store UPDATEs' WHERE for atomicity.
10. **Immediate `Cancel()` of a never-activated trial is a write-off.**
    During-trial cancels were already free; the post-trial-*lag* window
    (trial elapsed, activation scan behind, still `status=trialing`) used to
    bill prorated base + usage for a sub that never activated. Decided: no
    invoice (`activated_at IS NULL ∧ trial_end_at IS NOT NULL` guard) —
    matching the "you won't be charged" promise rather than billing on scan
    lag the customer can't see.
11. **`cancel_effective_at`** — a derived, read-only field on the
    subscription JSON and the `cancel_scheduled` event payload
    (trialing+flag → trial_end; active+flag → period_end; else cancel_at),
    computed at the store's scan choke point. Both existing UI surfaces
    showed the wrong date by re-deriving from the flag; they now read this.

## Accepted residuals

The spec's "one shared helper across all four sites" is implemented as one
service helper + one engine helper with verified-identical vocabulary — the
zero-peer-import rule forbids the engine importing the subscription service;
drift risk is future-facing and covered by the winner-once tests. OpenAPI
documentation for schedule-cancel / end-trial remains fully outstanding —
`api/openapi.yaml` documents neither endpoint today; it belongs to the
pre-existing OpenAPI-backfill deferral, not this change.


The UI-time race (dialog open while the trial expires) is inherent; the
handler surfaces the real post-write state and the status-CAS closes the
API-side window. A crash between the CAS commit and the post-commit webhook
loses that one event — consistent with the repo-wide dispatcher posture (no
Dispatch-in-tx; 19 justified instances, ADR-062 deferred).

## Test locks (mutation-verified)

Real PG: `TestActivateAfterTrial_SQLGuardBlocksScheduledCancel` (predicate
stripped → activates), `TestCancelAtTrialEnd_CASSemantics` (CAS stripped →
the rescinded customer gets terminated),
`TestScheduleCancel_RacesActivation_ExactlyOneMeaning` (forbidden
active+trial-flag state unreachable). Unit: scan routing (one `canceled`,
zero `trial_ended`, zero bills), boundary matrix, EndTrial/ExtendTrial
guards, engine no-bill regression (single-route mutation stays green —
defense in depth — double-route mutation fails), write-off both timings.
