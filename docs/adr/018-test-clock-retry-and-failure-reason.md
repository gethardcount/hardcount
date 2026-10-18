# ADR-018: Test-clock retry advance + persisted failure reason

**Status:** Accepted (amended 2026-05-05)
**Date:** 2026-05-04

## Summary

When a test-clock catch-up fails, the clock stores the reason in `last_failure_reason` (cut to 500 characters) and the dashboard shows it. `POST /v1/test-clocks/{id}/retry-advance` moves a clock from `internal_failure` back to `advancing` and runs the catch-up again. This is safe because catch-up bills only subscriptions that are still due. Amended 2026-05-05: setting `VELOX_TEST_CLOCK_INJECT_FAILURE` makes the next catch-up fail once, so the failure path can be tested. Status: active, amended; deleting a clock is now a hard delete ([ADR-086](086-simulated-data-lifecycle.md)), not the soft delete named below.

## Context

When the async test-clock catchup worker (ADR-015) errors, it
calls `Store.MarkFailed` which flips the clock to
`status='internal_failure'` — and there it sits. The pre-fix UI
copy on `TestClockDetail`:

> Catchup failed during last advance.
> Some invoices may have been generated before the failure. Inspect
> billing state, then **delete this clock to start a new simulation**.

Two problems caught by an operator who intentionally removed Stripe
credentials to exercise the failure path:

1. **Failure reason invisible.** `MarkFailed` stored only the
   status flip; the underlying error string was logged via slog
   but never surfaced. Operator had to grep server logs / audit
   entries to find out what went wrong.

2. **Only recovery path was DELETE + rebuild.** Punishing for
   transient failures — a Stripe Tax 503, the now-fixed
   tenant-id-on-ctx context bug, or an intentionally-removed-creds
   debug run — none of which warrant losing the simulation setup
   (customer + sub config + earlier successful advances).

Industry references walked:

- **Stripe Test Clocks** — failed clocks expose a "Retry advance"
  button. Their docs explicitly recommend retry over delete:
  *"the catchup is idempotent on subscriptions, so retrying from
  where it stopped is safe."*
- **Lago / Orb** — same: retry, don't force restart.
- **Recurly / Chargebee** — sandbox-time-travel features expose
  retry on transient errors.

Velox's `runCatchupLoop` is already idempotent on subs (only
processes rows whose `next_billing_at <= frozen_time`), so
resuming from where the previous attempt stopped is safe by
construction. The missing pieces were the persistence column
and the retry transition.

## Decision

Two coupled changes.

### Persist the failure reason

Migration 0075 adds `test_clocks.last_failure_reason TEXT`.
`Store.MarkFailed` now takes a `reason string` argument; the
worker captures `err.Error()` and passes it through. The
dashboard's `TestClockDetail` surfaces it in the existing
internal-failure card:

> Catchup failed during last advance.
> Reason: *stripe tax: provider 503 service unavailable*
> Some invoices may have been generated before the failure —
> review them below. Click **Retry advance** to resume from where
> catchup stopped, or delete this clock to start over.

The reason is truncated to ~500 chars before write (full payload
stays in structured slog for ops grep). Cleared on the next
successful advance OR on retry — the dashboard never shows
yesterday's error against today's failed run.

### Add the retry transition

New `Service.RetryAdvance`:

1. Validates current `status='internal_failure'`.
2. Calls new `Store.RetryFromFailed` — atomic CAS from
   `internal_failure` → `advancing`, clearing
   `last_failure_reason`. Frozen_time is unchanged (the catchup
   loop is idempotent and the operator's earlier Advance input
   is already stamped on the row).
3. Enqueues a `CatchupJob` on the existing async worker queue
   (or runs sync inline for tests with no queue wired).
4. Worker drains, runs `runCatchupLoop`, lands the clock back
   in `ready` (or back in `internal_failure` with a fresh reason
   if the underlying issue persists).

HTTP surface: `POST /v1/test-clocks/{id}/retry-advance`. 200 with
the now-advancing clock; 409 when current status isn't
internal_failure (refuses to retry from ready or already-
advancing).

State machine after this ADR:

```
ready ──Advance── advancing ──catchup ok── ready
                       │                     ▲
                       │                     │
                       └──catchup errored── internal_failure ──RetryAdvance──┘
                                                  │
                                                  └──Delete──→ (gone via ADR-016 soft-delete)
```

## Consequences

### Positive

- Operator recovers from a transient catchup failure with one
  click instead of rebuilding the simulation.
- Failure reason visible without leaving the dashboard — matches
  Stripe Test Clocks' UX and reduces "what happened?" support
  load.
- The state machine remains tight: no new states, just a new
  transition. internal_failure stays a real terminal-ish state
  (an operator who chooses NOT to retry can still delete; the
  clock can't accumulate hidden retry state).
- Idempotent retries by construction: `runCatchupLoop` only
  processes subs whose `next_billing_at <= frozen_time`, so a
  retry that runs against subs that were partially billed before
  the failure picks up exactly where the prior loop stopped.
  Already-billed subs have `next_billing_at` already advanced
  past the clock's frozen_time and are skipped.

### Negative

- The new column adds a few hundred bytes per failed-then-
  retried clock until the soft-delete sweeper (ADR-016) cleans
  them up. Negligible.
- `last_failure_reason` is reproducible structured slog content
  echoed into the database; if a sensitive error string
  (rare for the tax/billing path) ever leaks via this column,
  it's visible to dashboard users with test-mode access. The
  500-char truncation reduces but doesn't eliminate the
  surface; given test-mode access already implies high trust,
  this is acceptable.

## Compatibility

- API surface adds one endpoint; existing endpoints unchanged.
- Frontend: TestClockDetail's internal_failure card grows the
  reason line + Retry button. No other surface changed.
- `Store.MarkFailed` signature changed (gained a `reason string`
  param). Internal-only — no external callers; the `Service` and
  the catchup worker are the two callers, both updated.
- Migration 0075 is additive (one nullable column); rollback is
  a clean DROP COLUMN.

## Amendment 2026-05-05: manual-test injection knob

Operator review of the MANUAL_TEST FLOW TC2 catchup-failure bullet
surfaced that the documented triggers ("disconnect Stripe", "hit
the 10-min wall-clock cap") were unreliable in practice:

- Tax failures defer via the block-and-retry pattern (ADR-017);
  they do not propagate as catchup errors. So Stripe-disconnect
  doesn't reach `MarkFailed`.
- The 10-min wall-clock cap requires advancing very far on a
  fast-billing subscription so the catchup loop runs >10min
  wall-time. Hard to reproduce reliably and machine-speed-
  dependent.

The internal_failure UI path is real and worth verifying in any
integration smoke test — but the operator had no reproducible
trigger to do so.

Added `VELOX_TEST_CLOCK_INJECT_FAILURE` env knob:

- `runCatchupLoop` checks the env at the top of the function
  via `injectedCatchupFailureReason()`. If set, it returns an
  error with the value as the reason ("injected: <value>").
- One-shot: the helper unsets the env after reading, so a
  subsequent **Retry advance** click runs cleanly. This lets
  operators chain failure → retry-success in one session.
- Off by default; production processes don't set it. If they
  accidentally do, every advance fails until the env is unset
  (loud failure, easy to spot, no silent corruption).
- Test coverage: `TestAdvance_InjectFailureEnv` asserts the
  env-driven failure path + the one-shot clearing.

Stripe Test Clock has the same affordance ("force-fail simulation"
under their test-clock primitives). Mirroring it removes ambiguity
in the manual-test bullet without changing production semantics.

