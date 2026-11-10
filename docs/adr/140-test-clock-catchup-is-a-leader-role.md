# ADR-140: The test-clock catch-up is a leader-leased role

**Date:** 2026-11-11
**Status:** Accepted
**Supersedes:** [ADR-015](015-test-clock-async-catchup.md)'s in-process
queue, worker, start-up recovery and `503 catchup_queue_full`.
**Amends:** [ADR-114](114-leader-leases-tick-scoped-fencing.md) (a sixth
role), [ADR-028](028-billing-engine-period-loop-and-disjoint-flows.md) and
[ADR-029](029-fully-disjoint-test-clock-flows.md) (what drives the catch-up,
and its outer time bound).

## Summary

Advancing a test clock only marks it `advancing`. The catch-up (the work
that fell due in the time the clock skipped) is run by one replica at a
time: the holder of the `testclock_catchup` leader lease. It runs every
tenant's advancing clocks on a 2-second tick. Every write it makes to a clock row proves the
lease's fence, so a replica that lost the lease changes nothing. A replica
that stops mid-pass leaves the clock `advancing`, and the next leader runs
the pass again; the passes are safe to repeat. A deploy no longer strands an
advance or runs it twice.

This is part 1. Part 2, the stepped loop (#1276: an advance runs its work in
time order, one due instant at a time), builds on it.

## Context

ADR-015 ran the catch-up off the request on a buffered channel in each
process, drained by one goroutine, with `RecoverInFlight` re-queuing every
`advancing` clock when a process booted. The HA readiness review recorded
the cost as hazard 7 (`docs/dev/records/ha-readiness-2026-07-06.md`):

- A rolling deploy stops the old process while its advance runs. The new
  process is already up, so no boot re-queues the advance: the clock stays
  `advancing` until some replica restarts.
- Every booting replica re-queues every `advancing` clock with no claim, so
  two replicas can run one clock's catch-up at once.
- A full buffer parked the clock in `internal_failure` and answered `503
  catchup_queue_full` (#1511, #1517).

The test-clock design review of 2026-11-10 (`testclock-design-v2.md` in the
code-health plan) chose to fix this before stepping the catch-up through
time, because a stepped advance runs longer and resumes more often. Its first
draft added a per-clock owner and lease beside the one the repo already has.
A reviewer pointed out that ADR-114 already gives one fenced runner per job;
the catch-up is one more job.

## Decision

1. **A sixth leader role, `testclock_catchup`** (`leader.RoleTestClockCatchup`,
   migration 00226 adds it to the role CHECK and seeds its row). Its loop is
   `testclock.Service.RunCatchupLoop`, a `scheduler.Run` like the other five,
   started with them in `main.go` and always on, whatever the scheduler's
   settings: an advance would otherwise never finish.
2. **The tick** (`CatchupTick`, every `CatchupTickInterval` = 2s) lists the
   advancing clocks with `ListAdvancingForCatchup`: every tenant (TxBypass),
   the clock touched longest ago first, at most 20, in one statement that
   proves the fence (`leader_fence`). It runs one pass on each. Every write to
   a clock bumps `updated_at`, so a clock worked on this tick goes to the back.
3. **Fenced writes.** The catch-up's writes to a clock row
   (`SaveAdvanceSummary`, `CompleteAdvance`, `MarkFailed`) carry `AND
   leader_fence('testclock_catchup', token)`. A write that changes no row is
   one of two outcomes, and neither is a failure of the advance:
   - `ErrCatchupFenceLost`: the clock is still `advancing` but this run no
     longer holds the lease. The current leader runs the clock.
   - `ErrClockNoLongerAdvancing`: the clock is gone, or out of `advancing`
     (another run completed or failed it, or the tenant deleted it).
   The run stops, logs at INFO, and leaves the clock as it is. `Advance`,
   `RetryAdvance` and `Delete` are API writes and stay unfenced.
4. **The pass's time bound.** A pass runs under `CatchupPassDeadline` (10
   minutes, the queue worker's old whole-run bound; the pass still covers the
   whole advance). A pass that overruns it ends the advance in
   `internal_failure`, with a reason that names the clock's time. The tick's
   own ctx can end first: on a shutdown, or when the lease is lost (the
   heartbeat cancels a led tick's ctx within `leader.AbandonAfter`). Then the
   pass stops and the clock stays `advancing` for the next leader. Between phases the run
   checks its ctx only: the heartbeat and the fenced writes already cover a
   lost lease, so a fence query per phase would be a second guard on the same
   thing.
5. **Where an advance started from.** With no queue job there is nothing to
   carry the pre-advance time, so `MarkAdvancing` stores it: its statement
   sets `last_advance_summary = {"advanced_from": frozen_time}` from the row
   before it moves `frozen_time`. The pass reads it back. The summary's
   `advanced_from` is now right on a Retry (it was the target) and after a
   restart (it was zero).
6. **Deleted:**
   - the channel queue and its worker, `RecoverInFlight` and
     `parkAfterEnqueueFailed`;
   - the `503 catchup_queue_full` answer;
   - the inline catch-up `Advance` ran when no queue was wired;
   - the 10-minute whole-run timeout;
   - shutdown's stage 2 (the catch-up's tick now drains with the other loops
     in the last stage). Tests that relied on the inline run call
   `testclocktest.Drain`, which runs the tick on a leased ctx.

## Why this design

- **One mechanism.** ADR-114's lease already handles election, takeover,
  pause, cadence and fencing, with tests for each. The catch-up gets all of
  it by being one more role, with no second lease and no rescan ticker.
- **A stopped process is not a failure.** The old worker could only finish a
  pass or abandon it until a boot. Now the next leader repeats a stopped pass.
  That is safe because every phase guards its own writes:
  - the period compare-and-set (ADR-115);
  - the charge claim and its hold (ADR-127, ADR-137);
  - the dunning run's state compare-and-set;
  - the threshold fire-once probe;
  - the trial flip's and the pause resume's own predicates;
  - the grant retire's `WHERE` (ADR-071).

  Today's Retry advance already relies on the same guards.
- **The fence is on the clock row only.** The phases write billing rows the
  way the wall-clock billing tick does, with the same guards, so they need no
  fence of their own. The clock row is what decides whether the advance is
  done, failed or still running, so only its writes prove the lease.

## Alternatives considered

- **A per-clock owner and lease on `test_clocks`, with a rescan ticker on
  every replica** (the design review's v2). A second lease mechanism beside
  ADR-114's, with its own TTL, takeover and tests. Rejected for the one the
  repo has.
- **A session advisory lock per clock.** Holds a pool connection for up to
  ten minutes per running clock, cannot be fenced (ADR-114 #797), and strands
  behind transaction-mode poolers.
- **Keep the queue, add a periodic rescan.** Still runs a clock twice
  (a rescan on each replica has no claim), and keeps the 503.

## Consequences

- An advance starts within about one tick (2s) on an idle leader, or after
  the leader's current tick. It used to start at once. After a leader dies,
  `LeaseTTL` + `MaxPoll` (about 15s). The dashboard polls at 1.5s, so nothing
  it shows changes.
- All tenants' catch-ups run on one replica, one clock at a time, as each
  replica's single worker ran its own queue before. One large clock cannot
  hold the head of the list, because the list starts with the clock touched
  longest ago.
- **A deposed leader overlaps the next one for a moment.** Some phase work
  runs on contexts detached from the tick (a commit's 30-second budget, the
  5-minute steps budget after a tax retry's finalize, a charge's outcome
  stamp). That work can still be finishing when the next leader runs the same
  pass, exactly as on the billing role after a takeover, and the same per-row
  guards cover it. The deposed run's own writes to the clock row miss the
  fence.
- **The upgrade overlap.** During a rolling deploy from a version with the
  queue, the old replica's worker can still run a clock with its unfenced
  writes while the new leader runs it too. Two passes at once are covered by
  the per-row guards. The old worker's `CompleteAdvance` may land first; the
  new run then finds the clock out of `advancing` and stops with nothing
  written (`ErrClockNoLongerAdvancing`).
- A restore pauses `billing` and `dunning`, not this role: a restored
  `advancing` clock runs at once, as `RecoverInFlight` ran it at boot before.
  Test clocks are test mode only.
- The role can be paused and resumed like the others (`leader_pause`), which
  the runbook uses.

## References

- `internal/testclock/catchup.go`, `internal/testclock/service.go`
  (`runClock`), `internal/testclock/postgres.go` (`ListAdvancingForCatchup`,
  `transitionWithReason`, `fencedMiss`).
- `internal/platform/migrate/sql/00226_testclock_catchup_role.sql`.
- `internal/testclock/catchup_lease_integration_test.go`.
- Runbook: `docs/ops/runbook/test-clock-advance-hung.md`.
- HA readiness record, hazard 7: `docs/dev/records/ha-readiness-2026-07-06.md`.
