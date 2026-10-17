# Test-clock advance hung

A test clock is a pretend clock for test customers. Moving it forward (an
advance) bills the skipped time, so you can see a month of billing in
minutes. This page is for an advance that does not finish. Back to the
[runbook index](index.md).

## Symptom

- `test_clocks.status='advancing'` for more than 5 minutes.
- The operator sees the Advancing badge stuck in the dashboard.

## Why

- **The catch-up is working through many cycles.** A large jump on a
  monthly subscription can need dozens of billing passes.
- **Billing failed during the catch-up.** The clock then moves to
  `internal_failure`.
- **A deploy abandoned the advance.** On shutdown, a replica waits 30
  seconds for an advance in progress, then exits. The clock stays
  `advancing` until **any replica starts or restarts**, because the
  recovery runs once at boot, never on a schedule. In a rolling deploy the
  new replica has usually booted already, so nothing picks it up. See Fix
  step 3.

## Check

```sql
SELECT id, name, status, frozen_time, updated_at
FROM test_clocks WHERE status = 'advancing';

-- Catch-up progress: subscriptions on this clock
SELECT s.id, s.next_billing_at, count(i.id) AS invoices_generated
FROM subscriptions s
JOIN test_clocks tc ON tc.id = s.test_clock_id
LEFT JOIN invoices i ON i.subscription_id = s.id AND i.created_at > tc.updated_at
WHERE tc.id = '<clock_id>'
GROUP BY s.id, s.next_billing_at, tc.updated_at;
```

## Fix

1. If there is progress (invoices are being generated), wait. Large jumps
   take time.
2. If the clock is in `internal_failure`, retry the advance with the
   dashboard's **Retry advance** button, or
   `POST /v1/test-clocks/<clock_id>/retry-advance`. It moves the clock back
   to `advancing` and resumes the catch-up from where it stopped.
   Delete the clock only as a last resort: deleting a clock removes all of
   its simulated data.
3. If a deploy abandoned the advance, restart one replica. Until recovery
   runs on a schedule, this is the operator's job.

## Verify

The first query returns no rows, and the clock shows its new time in the
dashboard.

Related: [ADR-018](../../adr/018-test-clock-retry-and-failure-reason.md) (retry from
where it stopped) and [ADR-086](../../adr/086-simulated-data-lifecycle.md)
(what a delete removes).
