# Test-clock advance hung

A [test clock](../../glossary.md#testing-with-simulated-time) is a pretend
clock for test customers. Moving it forward (an advance) bills the skipped
time. That billing is the catch-up. One replica at a time runs every clock's
catch-up: the holder of the `testclock_catchup` role, on a 2-second tick
([ADR-140](../../adr/140-test-clock-catchup-is-a-leader-role.md)). Back to the
[runbook index](index.md).

## Symptom

- `test_clocks.status='advancing'` for more than 5 minutes.
- The operator sees the Advancing badge stuck in the dashboard.

## Why

- **The catch-up is working through many cycles.** A large jump on a
  monthly subscription can need dozens of billing passes. One pass may run
  for up to 10 minutes; a longer one ends the advance in `internal_failure`.
  The role runs one clock at a time, so an advance can also wait for other
  clocks' passes. While it does, the role's stall alert
  (`hardcount_leader_last_tick_age_seconds`) can fire with nothing wrong.
- **Billing failed during the catch-up.** The clock then moves to
  `internal_failure`, with the reason in the dashboard's banner.
- **The `testclock_catchup` role is not running:** it was paused, or no
  replica can take it. A replica that stops mid-pass (a deploy, a crash) is
  not a cause on its own. The clock stays `advancing`, and the next replica
  to take the role runs the pass again within about 15 seconds.

## Check

```sql
SET app.bypass_rls = 'on';
SELECT id, name, status, frozen_time, updated_at, last_failure_reason
FROM test_clocks WHERE status = 'advancing';

-- Is the catch-up role running? last_tick_age_s should stay under a few
-- seconds, or show a tick in progress (held = true).
SELECT role, held, holder_id, last_tick_age_s, paused_at, paused_by, pause_reason
FROM leader_status WHERE role = 'testclock_catchup';

-- Catch-up progress. Run it twice, a minute apart: the catch-up is working
-- if the invoice count grows or the earliest next billing moves forward.
SELECT count(DISTINCT s.id) AS subscriptions,
       min(s.next_billing_at) AS earliest_next_billing,
       count(i.id) AS invoices
FROM subscriptions s
LEFT JOIN invoices i ON i.subscription_id = s.id
WHERE s.test_clock_id = '<clock_id>';
```

Compare the two runs, not the times in one run with the wall clock: a
clock's invoices are dated in simulated time.

## Fix

1. If there is progress (the invoice count grows or the earliest next
   billing moves forward between the two runs), wait. Large jumps take
   time.
2. If the role is paused (`paused_at` is set), resume it once the reason is
   dealt with: `SELECT leader_unpause('testclock_catchup');`. If no replica
   holds it and its last tick is old, follow
   [A background job is stuck](background-job-stuck.md).
3. If the clock is in `internal_failure`, retry the advance with the
   dashboard's **Retry advance** button, or
   `POST /v1/test-clocks/<clock_id>/retry-advance`. It moves the clock back
   to `advancing`, and the next tick resumes the catch-up from where it
   stopped. Delete the clock only as a last resort: deleting a clock
   removes all of its simulated data.

## Verify

The first query returns no rows, and the clock shows its new time in the
dashboard.

Related: [ADR-140](../../adr/140-test-clock-catchup-is-a-leader-role.md) (who
runs the catch-up), [ADR-018](../../adr/018-test-clock-retry-and-failure-reason.md)
(retry from where it stopped) and
[ADR-086](../../adr/086-simulated-data-lifecycle.md) (what a delete removes).
