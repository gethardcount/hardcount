# ADR-064: Dunning-run creation — triggered-primary + derived-backstop

**Date:** 2026-07-02
**Status:** Accepted

## Summary

When a charge fails, Velox starts the invoice's dunning run (its schedule of payment retries) at once, and a background sweep creates any run that was missed. Both paths call `StartDunning`, which creates at most one run per invoice, so the sweep safely repairs the gap a crash leaves between marking the invoice failed and starting dunning. The retry schedule is anchored on the invoice's own timestamps, so a run the sweep creates late is the same as one created at once. Storing the retry date as a column on the invoice, as Stripe does, is rejected for good: dunning is its own domain, and test-clock invoices still need the inline path. Status: active.

## Context

When a payment fails, Velox creates a `dunning_run` — a stateful collection
campaign (state `active`/`resolved`/`escalated`, `attempt_count`, `next_action_at`,
a per-run frozen policy binding, an append-only events timeline, the operator
"in collection" badge). `dunning.StartDunning` (`internal/dunning/service.go:246`)
is the single creation function; it is **idempotent per invoice**
(`GetRunByInvoice` returns the existing run regardless of state, backed by the
0085 `UNIQUE(tenant_id, invoice_id)` index).

Historically `StartDunning` fired only as a **triggered side-effect** on failure:
inline in the synchronous charge path (`chargeInvoice` → `StartDunning`) and
post-commit in `SettleFailed` (`internal/payment/settlement.go`, best-effort,
behind the `firstForThisPI` gate, after `MarkPaymentFailedReportingTransition`
commits). The `SettleFailed` case is a **dual-write**: the invoice is committed
`failed` in one transaction and dunning is started in a separate step, so a crash
— or an exhausted retry, or a same-PI webhook redelivery that skips the gate —
between the two leaves the invoice `failed` with **no run**, and collection
retries are silently never scheduled (a revenue-recovery leak).

PR #328 closed that reliability gap with a **`dunning_backfill` reconciler**
(`billing.Engine.EnrollFailedWithoutDunning` + `invoice.ListFailedWithoutDunningRun`):
a state-agnostic sweep of finalized, still-owed, `failed` invoices with **no**
dunning run, which re-drives the idempotent `StartDunning`. It is state-agnostic
(a run in *any* state — including `resolved`/`escalated` — excludes the invoice),
exactly-once (0085 + `GetRunByInvoice`), and excludes clock-pinned invoices (ADR-029).

The open question this ADR settles: **should dunning move to a fully "derived"
model** — the run's existence purely a projection of invoice state, à la Stripe's
`next_payment_attempt` scalar column, with the inline trigger removed so there is
"one creator" and "no run to lose"? This was investigated with an adversarial
design panel. The answer is **no**, and the reasoning is load-bearing enough to
record so a future session does not re-chase it.

## Decision

**Ratify the two-path model as the intended, durable design:** the triggered
inline `StartDunning` is the **primary (fast) path**, and the derived
`EnrollFailedWithoutDunning` sweep is the **always-on, self-healing backstop**.
**Correctness never depends on the trigger firing.**

The load-bearing invariant any future change **must preserve**:

> An inline-created run and a sweep-created run for the same invoice are
> **observably identical modulo latency.** This holds because (a) `StartDunning`
> is idempotent per invoice (`GetRunByInvoice` + 0085 UNIQUE), and (b)
> `next_action_at` is anchored on the invoice's own timestamps
> (`dunningFailureAt` → `IssuedAt`/`BillingPeriodEnd`), **not** on creation
> wall-clock. So whether the run is born instantly (trigger) or on a later
> scheduler tick (backstop), its retry schedule is byte-identical.

That equivalence **is** the derivability the "derived dunning" goal is after: the
run's **existence** is derivable (the state-agnostic `NOT EXISTS` sweep re-drives
it) and its **schedule** is derivable (invoice-anchored). "No run to lose" is
already achieved. **Derivability is not the same as single-mechanism** — Velox
gets the self-healing-by-construction property without collapsing the campaign
into a scalar.

Two structural facts make "one creator" impossible or undesirable, and are
recorded here as **reasons, not accidents**:

1. **Clock-path duality (ADR-029).** The wall-clock reconciler deliberately
   *excludes* clock-pinned rows (`test_clock_id IS NOT NULL`), because simulated
   subscriptions are driven only by operator `Advance`. A clock-pinned declined
   invoice therefore gets its run from the **inline** path *inside* the Advance
   (a test clock has no dropped-webhook gap — a crash is just a re-click). Removing
   the inline trigger would *force a new* `EnrollFailedWithoutDunningForClock`
   catchup variant — **more** creation paths, not fewer. "Single creator" is
   illusory.
2. **Co-instant audit guarantee (ADR-035 / flow X1).** The `payment.failed`
   event and the "retry scheduled" timeline row are asserted to land at the
   *same simulated instant*. Moving run creation to a later tick breaks that.

## Alternatives rejected

1. **Full-derive / `next_payment_attempt`-on-invoice (the Stripe scalar model).**
   Stripe can carry the retry schedule as a scalar column on the invoice because
   its dunning lives in the *same* service. Velox deliberately made dunning a
   stateful campaign *aggregate* in its own domain (escalation, resolution, policy
   binding, per-attempt events, the runs tab). Collapsing that into an invoice
   column would either lose the campaign richness or drag dunning's schema onto
   `invoices` — violating the zero-cross-domain rule (`invoice` may not import
   `dunning`; it would have to *own* retry). And clock-pinning still forces a
   second creation path. It buys *less*, not more.
2. **Single-creator / remove the inline trigger.** Illusory (forces the ForClock
   variant above), and it regresses three properties the two-path model keeps:
   ADR-029 clock-dunning-inside-Advance, instant operator visibility (the run row +
   red badge + `dunning.started` webhook would lag the failure by up to one
   scheduler tick — currently 5 min), and the ADR-035 co-instant audit guarantee.
   Net ~300–500 LOC for negative pre-launch ROI: it adds *zero* money safety (#328
   already sealed the leak) and only swaps an instant-primary for a delayed-primary.
3. **Keep-triggered-only, no backstop (pre-#328).** A lost trigger (post-commit
   crash / exhausted retry / same-PI redelivery skip) strands the invoice
   `failed`-forever — the exact revenue-recovery leak the backstop closes. This is
   the option the ratified design *beats*.

## Consequences

- Correctness of "every failed, owed invoice reaches a dunning terminal" is
  guaranteed from durable invoice state, independent of whether any trigger fired.
- In the common case the inline trigger gives **instant** operator feedback; in
  the rare crash/dropped-webhook case the backstop self-heals within **one
  scheduler tick (≤5 min)**. First retry is grace-days out regardless, so the
  customer never sees the difference.
- Exactly-once, no-cancel-paid (#325 paid-pre-check + `exhaustRun` late re-check),
  never-re-dun-resolved (state-agnostic `NOT EXISTS`), and simulated-time
  anchoring are all preserved — they live in the run state machine, which this
  decision does not touch.
- **Accepted residual:** the backfill sweep's cool-off rides on the general-purpose
  `updated_at` column (an unrelated invoice mutation bumps it, resetting the
  cool-off), and the failure-instant is derived by two divergent heuristics
  (`simulatedFailureAt` clamps into the billing period; `dunningFailureAt` uses
  `IssuedAt`-else-`period_end`). Both affect only backstop *latency* and a sub-day
  cosmetic first-retry offset — never correctness (the `NOT EXISTS` is the real
  gate). Documented, not fixed.

## Cheap-strengthening triggers (deferred, not pre-launch work)

- **A durable `collection_failed_at` anchor** (one migration) — a single
  authoritative failure-instant read by both the sweep cool-off and
  `next_action_at`, retiring the `updated_at` proxy and both divergent heuristics.
  Build when a *second* consumer of the failure-instant appears, or if
  mutation-driven cool-off starvation is ever observed.
- **`payment.failed` event emitted in the fail-tx** via the transactional outbox
  (symmetric to `payment.succeeded` in `MarkPaidCardSettlementTransition`; the
  failed-*email* stays post-commit by design, like the receipt email). The one
  genuinely-first-practice slice adjacent to this area — **built immediately after
  this ADR** (enqueued inside `MarkPaymentFailedReportingTransition`, gated on
  `firstForThisPI`).
- **ADR-062 obligation queue.** When it lands, its drainer becomes the single
  creator and removing the last inline trigger is nearly free — at which point
  revisit whether the instant-feedback / co-instant-audit tradeoffs still justify
  keeping inline.

The invoice-column collapse (alternative 1) is **permanently rejected**, not deferred.
