# A background job is stuck

Velox runs its billing, payment retries, webhooks and emails in background
jobs. This page is for when one of them seems to have stopped. Back to the
[runbook index](index.md).

## How the jobs share work

Five background jobs must run on exactly one replica at a time. Each is
called a role:

| Role | Runs every |
|---|---|
| `billing` | 1 hour in production, 5 minutes when `APP_ENV=local` |
| `dunning` (payment retries) | 1 hour in production, 5 minutes when `APP_ENV=local` |
| `webhook_outbox` | 2 s |
| `email_outbox` | 5 s |
| `webhook_delivery` (the webhook retry worker) | 30 s |

Each role is one row in the `leader_leases` table. Think of it as a key on
a hook: a replica takes the key to run one tick and hangs it back when the
tick ends.

- To run a tick, a replica takes that row for **10 seconds** (the lease),
  renews it every **3 seconds** while its work runs, and releases it when
  the tick ends.
- The clock is the database's, so the replicas never need to agree on the
  time.
- Nothing is held for longer than one tick. Whichever replica polls first
  after the role is due (one interval after the last tick **ended**) takes
  the next one. There is no standing leader, so there is nothing to fail
  over.

Three things keep a tick from running twice:

- **A dead holder cannot keep the row.** With no renew for 10 s, the lease
  expires on the database clock, and any replica takes it on its next poll
  (every 5 s at most). At worst a role is silent for **15 s** after a crash,
  network partition, VM pause or `SIGSTOP`.
- **A holder that cannot renew stops itself** after 6 s without an
  acknowledged renew. That is inside the 10 s, so it is gone before another
  replica can start.
- **Every claim statement re-checks the tick's token.** The row carries a
  number that goes up on every acquire (the fence token). A tick's claims
  (due subscriptions, due dunning runs, outbox rows, pending deliveries)
  check it with `leader_fence(role, token)`, so a tick that outlived its
  lease claims nothing. Work already claimed is protected by a
  compare-and-swap update when it is written.

## Symptom

- The `scheduler` check in the `/health/ready` body starts with
  `degraded`. The status code stays 200.
- Subscriptions due for billing are not being invoiced, and the
  `velox_billing_cycles_total` rate drops to 0.
- `velox_leader_last_tick_age_seconds{role}` is above 3× the role's
  interval.
- An outbox keeps growing, or webhook retries stop.

Before you treat a gap in invoices as an outage, check the tick interval
in [Scheduler interval and batch size](#scheduler-interval-and-batch-size).
`/health/ready` reports `degraded` only after 2× that interval.

## Why

- **A long transaction holds row locks**, for example a tenant with
  millions of usage events on a single subscription.
- **The database primary failed over**, and connections were lost
  mid-tick.
- **An operator paused the role** (see [Pause a role](#pause-a-role-on-purpose)).
- **A tick is wedged**: it keeps renewing its lease (its heartbeat is
  alive) while its work is stuck.
- **No replica is polling** for the role.
- **It is not a crash loop from a panic.** A tick that panics is
  recovered and the poll loop keeps running, so the `/health/ready`
  scheduler check stays `ok`. A panicking tick shows up instead as
  `velox_leader_lease_lost_total{reason="panicked"}` and a growing
  `velox_leader_last_tick_age_seconds{role="billing"}`, with the stack in
  the `scheduler panic recovered` ERROR log.

A long tick is not a stuck one. A billing tick bills every subscription
that is due before it ends, so on a busy billing day it can run for hours.
The next tick starts one interval after it ends.

## Check

1. **Look at the roles.**

   ```sql
   SELECT role, held, holder_id, expires_in_s, last_tick_holder,
          last_tick_age_s, paused_at, paused_by, pause_reason
     FROM leader_status ORDER BY role;
   ```

   **You see:** one row per role.

   | Column | Meaning |
   |---|---|
   | `held` | A tick is running right now (lease not expired) |
   | `holder_id` | `host:pid:hex` of the replica running it, for observing only |
   | `expires_in_s` | Seconds until the lease lapses if the holder stops renewing |
   | `last_tick_holder`, `last_tick_age_s` | Which replica last finished a tick, and how long ago. **This is the cluster-wide fact** |
   | `paused_*` | An operator paused the role (see below) |
   | `not_before`, `not_before_in_s` | The role is held back for at most 60 s, after a tick ended without completing and released its own lease (its work panicked, or its heartbeat timed out before anyone took over). Such a tick never counts as completed, so `last_tick_*` is not changed by it |

2. **Read the answer.**
   - Paused: see [Pause a role](#pause-a-role-on-purpose).
   - `held` true and `last_tick_age_s` growing: a wedged holder. For
     billing, check the due gauge first (step 3).
   - `held` false and `last_tick_age_s` growing: no replica is polling.
     Check every replica's logs and `/health/ready`.

3. **For billing, check whether the tick is draining.** While a long tick
   runs, `velox_billing_due_subscriptions` falls page by page.
   - If it is falling, the tick is working through a backlog. A restart
     only restarts the drain.
   - If it is flat and `velox_billing_failing_subscriptions{state="retrying"}`
     is zero, the tick is wedged.
   - Subscriptions whose billing is failing are not in the due gauge. The
     run skips them while they wait, then attempts them again.
     Subscriptions that failed together come due together, so a tick can
     be busy retrying them while the due gauge is flat. Fresh
     `bill subscription failed` log lines mean it is working, not wedged
     (see [Billing failing](billing-failing.md)).
   - `velox_billing_cycle_duration_seconds` is recorded only when a tick
     ends. It shows how long past ticks took, not the one running now.

4. **Look for long queries and the scheduler's last run.** Run the SQL in
   `psql` and the `curl` line in a shell:

   ```sql
   -- Long-running queries
   SELECT pid, now() - query_start AS duration, state, query
   FROM pg_stat_activity
   WHERE state = 'active' AND query_start < now() - interval '30 seconds'
   ORDER BY duration DESC;
   ```

   ```bash
   # Scheduler last-run timestamp (in the response body)
   curl -s http://localhost:8080/health/ready
   ```

5. **Search the logs.** `scheduler tick led` is logged on the replica that
   ran a tick (INFO for billing and dunning, DEBUG for the fast roles).
   `leader: lease lost` is logged at ERROR.

## Fix

1. **A panic.** Search the application logs for `scheduler panic recovered`.
   A panic in the tick repeats on whichever replica leads next, so a
   restart does not fix it: find the input in the stack, and fix or remove
   it. A panic while billing one subscription is also logged as
   `bill subscription panicked` with the subscription id. It repeats only
   when that subscription's wait ends (see
   [Billing failing](billing-failing.md)).
2. **A wedged holder.** Find the holder in `holder_id` and restart that
   replica.
3. **A long query.** Cancel it with `SELECT pg_cancel_backend(<pid>)` if
   appropriate.
4. **One tenant dominates the batch.** The batch size is fixed at 50
   subscriptions per tick and cannot be changed with an environment
   variable. Drain that tenant on demand with `POST /v1/billing/run` (per
   tenant; it loops until empty).
   - It is safe to run while the leader's tick is in flight.
   - A drain may report 0 for a subscription the leader is committing at
     that instant. The leader's commit bills it, and the two can never
     both bill a period.
   - A drain skips a subscription whose billing is failing and is waiting
     for its next attempt or parked. See [Billing failing](billing-failing.md).
   - A drain does not close a period that is still inside its
     `INVOICE_GRACE_PERIOD`. See
     [Invoice timing and late usage](invoice-timing-late-usage.md).

## Verify

`SELECT * FROM leader_status` shows a small `last_tick_age_s` for the role,
and `velox_leader_last_tick_age_seconds{role}` falls back below 3× the
role's interval.

After a database failover the roles carry on, because the row moved with
the data. Expect at most one `lease_lost` per role from the ticks that were
in flight.

## Pause a role on purpose

```sql
SELECT leader_pause('email_outbox', 'your-name', 'why');   -- returns true
SELECT leader_unpause('email_outbox');
```

While a role is paused, no replica takes it. Work queues up and resumes on
unpause: an outbox drains, and a billing cycle catches up on its next tick.
A pause survives restarts, because it is a row, not a process. So
**unpause is part of the change**, and `velox_leader_paused` is the
reminder.

Do not edit `leader_leases` by hand beyond these two functions.

- Deleting a row is harmless. The next poll's acquire recreates it with a
  fresh token, seeded from the clock so it lands above any token ever
  issued.
- The fence token and the pause state live in that row. A hand-set token
  can make a running tick's claims return nothing, with no error.

## Metrics for the roles

| Metric | Alert when |
|---|---|
| `velox_leader_last_tick_age_seconds{role}` | > 3× the role's interval. No replica has finished a tick. A single replica's liveness gauge (`velox_scheduler_last_run_timestamp_seconds`, which followers also stamp) cannot give this signal |
| `velox_leader_held{role}` | Stays 1 while `last_tick_age` grows: a tick is wedged but still renewing. See [Check](#check) step 3 for billing |
| `velox_leader_paused{role}` | 1 outside a planned window |
| `velox_leader_lease_lost_total{role,reason}` | Any increase, except `reason="paused"`. See [Lease-lost reasons](#lease-lost-reasons) |
| `velox_leader_status_scrape_errors_total` | 1: the view could not be read. The database is the problem, not the roles |

The billing due gauge, `velox_billing_due_subscriptions`, counts due
subscriptions whose billing is not failing. Ones with a saved failure are in
`velox_billing_failing_subscriptions` instead. The tick still attempts those
once their wait has passed, so it can be busy on them while the due gauge is
flat.

### Lease-lost reasons

| `reason` | Meaning |
|---|---|
| `paused` | An operator paused the role mid-tick. Expected; exclude it from alerts |
| `heartbeat_timeout` | The holder could not renew in time and cancelled its own tick |
| `takeover` | A renew found another holder |
| `released` | A renew found the row already released |
| `panicked` | The tick's work panicked and ran nothing past the panic. See below |

For `heartbeat_timeout`, `takeover` and `released`, correctness held (the
fence token and the row compare-and-swap). Find out why the leader lost
its lease: a frozen process, a database stall, or a connection pooler
problem.

After `panicked`, or a `heartbeat_timeout` that nobody took over, the role
(not just this replica) waits `min(interval, 60 s)` before any replica
leads it again. After a `takeover` the new holder is already running it.
After `paused`, nobody leads until it is unpaused.

**A panic repeats.** The stack is in the `scheduler panic recovered` ERROR
log, and the same panic comes back every `min(interval, 60 s)` until the
bad input or code is fixed. Restarting does not help.

**A panic while billing one subscription is the exception.**

- It is saved as a billing failure on that subscription, and later ticks
  skip the subscription until its wait ends.
- Each attempt that panics again ends that one tick early. The tick that
  follows about 60 s later skips the subscription and does the rest of the
  work. So the page fires once per attempt, not every 60 s.
- That is at most eight times over 3 days 17 hours, spaced on the wait
  ladder (15m, 1h, 4h, 12h, 24h three times). Then the subscription parks
  and the panic stops. On the hourly production tick the first three
  attempts land on three ticks in a row.
- The subscription shows up in `velox_billing_failing_subscriptions`, and
  its id and stack are in the `bill subscription panicked` ERROR log. See
  [Billing failing](billing-failing.md).

## A subscription is due but not billed

`velox_billing_oldest_due_age_seconds{mode}` above 2× the billing interval
means a subscription has been due for billing that long, and its billing is
not saved as failing. "Due" starts when `INVOICE_GRACE_PERIOD` has passed
after the period end, so a subscription inside its grace is not counted
(see [Invoice timing](invoice-timing-late-usage.md)).

There are two causes:

- **Billing is behind.** `velox_billing_due_subscriptions` is large and
  falling on a busy day, and it catches up.
- **One subscription's attempts are interrupted, or hit connection errors,
  on every tick.** The count is small and flat. Its id is in the
  `bill subscription failed` ERROR log, with `class` `interrupted` or
  `transient`.

A subscription that fails to bill for any other reason is not counted here.
It is in `velox_billing_failing_subscriptions`; see
[Billing failing](billing-failing.md).

## Scheduler interval and batch size

The tick interval and batch size are compiled in, not set by environment
variables:

- The scheduler ticks every **1 hour** in staging and production, and every
  **5 minutes** only when `APP_ENV=local`.
- It processes a fixed **50 subscriptions per tick** (`cmd/velox/main.go`).

Drain a tenant with a backlog on demand with `POST /v1/billing/run`, not by
tuning these. It loops until that tenant has nothing left that it will
attempt.

- Like the tick, it closes a period only once `INVOICE_GRACE_PERIOD` has
  passed after its end (see [Invoice timing](invoice-timing-late-usage.md)).
- It skips subscriptions whose billing is failing while they wait for their
  next attempt or are parked (see [Billing failing](billing-failing.md)).
- A drain running under the leader's tick may report 0 for a subscription
  the leader is committing at that instant, because its due fetch skips
  locked rows. The leader's commit bills it.

Watch `velox_billing_cycle_duration_seconds` to check that each tick fits
inside the interval. One replica holds the `billing` role per tick and
renews it while it works. So if a tick runs long, the next tick starts one
interval after the long one ends, on whichever replica polls first. There
is no collision and no lock wait, only a growing backlog.
`velox_leader_last_tick_age_seconds{role="billing"}` is the cluster-wide
lag.

## Poolers and migrations

Every lease statement is self-contained, so the server is safe behind
PgBouncer in transaction mode and behind RDS Proxy. Migrations are not.
They hold a transaction-scoped advisory lock on a dedicated connection, and
some run outside a transaction. Run them on a direct or session-mode
connection ([Postgres requirements](../postgres-requirements.md)).

## Drills

- `scripts/partition-drill.sh` cuts a holder's network link and reports
  when the row expires and another replica takes it: **9 s** measured
  (2026-08-30).
- `TestLeaseFailover_SIGKILLedHolderReplacedWithinTTL` covers a process
  that dies: **9.5 s** across five runs. It also checks that no other
  replica takes the role while the dead holder's lease could still be live.
- The advisory-lock drill this replaced measured 95 s and depended on TCP
  keepalives. This one depends only on the database clock.
- `scripts/pgbouncer-test.sh` runs the integration suite behind PgBouncer
  in transaction mode, the same job CI runs.

Related: the design and its reasons are in
[ADR-114](../../adr/114-leader-leases-tick-scoped-fencing.md); the
safe concurrent drain is in
[ADR-115](../../adr/115-one-closer-for-the-billing-period.md).
