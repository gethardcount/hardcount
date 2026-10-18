# ADR-015: Test-clock advance runs catchup asynchronously

**Status:** Accepted
**Date:** 2026-05-04

## Summary

Advancing a test clock (a pretend clock for test customers) returns at once with status `advancing`, so a long jump never hits an HTTP timeout. A background worker then runs the catch-up: it bills every period the jump skipped and sets the clock back to `ready`. Each replica has one in-process worker with a 100-job queue and a 10-minute limit per advance, and a failure sets `internal_failure` ([ADR-018](018-test-clock-retry-and-failure-reason.md) adds retry). At boot, each replica re-queues clocks left in `advancing`. Status: active.

## Context

Velox test clocks let an operator fast-forward simulated time on a
group of subscriptions. When the clock advances, the billing engine
must "catch up" — process every billing cycle that closed during the
jump and emit the corresponding invoices. This is the entire point
of test clocks; without it, advancing time would leave the operator
with no observable result.

Initial implementation (commit before this ADR) ran the catchup
**synchronously inside the `POST /v1/test-clocks/{id}/advance` HTTP
handler**. The handler called `runCatchup` directly, which looped
the engine up to 120 times until no subs were due, then returned
the post-`CompleteAdvance` clock to the caller.

That shape works at zero customers but is the wrong shape for
production:

1. **HTTP timeout cliffs.** Advancing 12 months on a monthly sub
   with 10 subs on the clock can produce ~120 engine passes. Common
   load-balancer timeouts (Cloudflare 100s, AWS ALB default 60s,
   Heroku 30s) cut the request before the loop finishes. Operator
   sees a 504; clock is left mid-flight in `status='advancing'`.
2. **HTTP worker starvation.** Long sync handler holds an API
   server worker. Multiple concurrent advances monopolise the pool.
3. **UX coherence.** Sync forces a blocking spinner. Operator
   can't navigate away or check on it from another tab.
4. **Resume-after-restart.** A `kubectl rollout` mid-catchup leaves
   the clock stuck in `advancing` with no automatic recovery.

Industry reference points (`feedback_reference_platforms` memory):

- **Stripe Test Clocks** — async. POST returns 202 with
  `status: "advancing"`; background workers run the catchup;
  status flips to `ready` when done. Documented behaviour.
- **Lago, Orb, Recurly, Chargebee** — same pattern; HTTP handler
  does the state transition, a worker does the work.

## Decision

Test-clock advance is asynchronous. The HTTP handler:

1. Validates inputs.
2. Calls `Store.MarkAdvancing` — atomic CAS from `ready` to
   `advancing`, simultaneously updating `frozen_time`.
3. Calls `CatchupQueue.Enqueue(CatchupJob{TenantID, ClockID})`.
4. Returns the `advancing` clock immediately (200 OK with the new
   `frozen_time` and `status: "advancing"`).

A dedicated `CatchupWorker` goroutine drains the queue. For each
job:

1. Wraps a fresh `context.Background()` with a 10-minute
   wall-clock timeout (`CatchupTimeout`) and pins
   `WithLivemode(false)` (test clocks are test-mode-only by
   table CHECK constraint).
2. Calls `Service.RunCatchup(ctx, job)` — the same loop that
   used to run inline.
3. On success: `Store.CompleteAdvance` → `status='ready'`.
   On error or timeout: `Store.MarkFailed` →
   `status='internal_failure'`. The operator can inspect partial
   results and delete the clock to recover.

### Boot recovery

`Service.RecoverInFlight` runs once at server start: scans
`test_clocks WHERE status='advancing'` (RLS-bypassed because it's
cross-tenant) and re-enqueues each. Idempotent — `runCatchupLoop`
only processes subs whose `next_billing_at <= frozen_time`, so
resuming partial work just continues from where the previous
process stopped.

### Queue implementation

In-process buffered Go channel + single worker goroutine. Buffer
of 100 jobs. On full, `Enqueue` returns `ErrQueueFull` and the
HTTP handler flips the clock to `internal_failure` so the operator
gets visible feedback rather than a clock stuck in `advancing`.

Single-worker on purpose — concurrent catchups against the same
tenant fight for the same RLS partition and the same DB rows;
serialising avoids deadlocks and there's no per-clock isolation
that parallelism would buy us at expected operator volumes.

### UI coupling

The dashboard already polls `/v1/test-clocks/{id}` every 1.5s while
`status === 'advancing'` (`TestClockDetail.tsx:60`). No frontend
change needed — the polling cadence picks up the worker-driven
transition automatically. Spinner text changes from "Advancing…"
in a modal to a non-blocking badge while the operator can navigate
freely.

## Consequences

### Positive

- HTTP advance returns in milliseconds regardless of jump size.
- Operator can navigate away; status badge updates on next poll.
- Server restarts mid-catchup recover automatically.
- No HTTP-timeout-induced stuck clocks.
- Same observable result for callers (status field, frozen_time,
  generated invoices) — only the timing of the transition changes.

### Negative

- One more boot-time goroutine to manage. Lifecycle wired into the
  same `workers sync.WaitGroup` pattern as the existing scheduler
  + webhook retry worker, so the operational shape is consistent.
- In-process queue means a single-replica view of jobs. Multi-
  replica self-host deployments (Helm chart) would have one worker
  per replica, each draining the same DB-backed clock state — not
  a problem because `MarkAdvancing` is a CAS; only the replica
  that won the CAS for a given clock can transition it. When
  multi-replica becomes a real concern (named DP demands
  HA-self-host), swap the in-process queue for Postgres
  `LISTEN/NOTIFY` or a Redis stream without touching the service
  contract — the `CatchupQueue` interface keeps the seam.
- Boot recovery scans across all tenants (RLS-bypassed). Bounded
  to 1000 rows; at expected volumes there will rarely be more
  than 0–1 stuck clocks. If recovery itself errors (e.g. DB
  briefly unavailable on boot), it logs loudly and continues —
  operators can manually delete stuck clocks if recovery
  permanently fails.

## Compatibility

- API surface unchanged. `POST /v1/test-clocks/{id}/advance`
  still returns 200 with the clock JSON. The clock's `status`
  field carries the difference: previously always `ready` on
  return; now `advancing` until the worker completes.
- Clients that polled `/v1/test-clocks/{id}` continue to work —
  the dashboard already does this on a 1.5s cadence.
- Any caller that asserted `status === 'ready'` immediately after
  the advance response must update to poll until the transition.

## Notes

- `MaxAdvanceCatchupLoops = 120` is retained as a within-process
  safety cap. The 10-minute wall-clock timeout
  (`CatchupTimeout`) is the new outer bound.
- The `Service.SetCatchupQueue` setter is the production wiring;
  the sync fallback path (`s.queue == nil`) remains for narrow
  unit tests that want to assert end-state synchronously without
  standing up the worker.
