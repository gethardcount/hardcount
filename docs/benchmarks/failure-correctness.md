# Correctness under failure

> **Record of a 2026-08-15 run, kept as measured (status checked 2026-10-17).** Since
> [ADR-115](../adr/115-one-closer-for-the-billing-period.md) (2026-08-30), every
> period close also locks the subscription row and checks its period first, so the
> unique index below is now the second guard, not the only one. The negative
> control has not been re-run on that code. `hardcount-doctor` now has 32 checks, not 28.

What happens to your invoices when the billing process dies mid-run.

**Result.** We killed the billing process with `SIGKILL` mid-run, sweeping the
kill across five points (one kill per run). We also raced four billing-engine
instances (goroutines in one test process) on the same 40 subscriptions. Every
run ended with 0 duplicate invoices, 0 lost invoices and 0 cents of drift. The
negative control is the same race with the unique index removed. One run billed
$2,575.00 instead of $1,000.00 (a second billed $3,225.00), and every leader
reported success.

**Date:** 2026-08-15 · **Commit:** `cde68ede` (#798)
· **Hardware:** developer laptop, Postgres 16 in Docker, single node.

---

## The guarantee

Two guards stop a second cycle invoice. Since ADR-115 each period close locks
the subscription row and checks its period. Behind that, a partial unique
index refuses a duplicate: a uniqueness rule the database enforces only on
rows matching the index's `WHERE` clause. The runs below measured the index
alone, on code from before ADR-115.

```sql
CREATE UNIQUE INDEX idx_invoices_billing_idempotency
  ON invoices (tenant_id, subscription_id, billing_period_start, billing_period_end)
  WHERE status <> 'voided' AND source_plan_changed_at IS NULL;
```

The `WHERE` clause scopes the rule to non-voided, regular billing-cycle
invoices. Since migration 00215 the index is
`idx_invoices_billing_period_dedup`, and its `WHERE` clause also leaves out
manual invoices (`billing_reason = 'manual'`). The runs below used the index
above. So a second live cycle invoice for a period that already has one
cannot be committed. The rule is "cannot be", not "is unlikely to be", and that
difference is the whole result.

The negative control below is the same run with the index removed, and it must
produce duplicates. That control is what makes this a measurement instead of a
claim.

---

## 1. Leader killed mid-run

The leader is the one billing process allowed to run the billing cycle. A real
billing process bills a seeded tenant of **40 due subscriptions** at $25.00
each. Partway through, it is `SIGKILL`ed. It gets no graceful shutdown, no
deferred unlock, and no chance to finish the row it was on. A second process
then runs the same cycle.

The kill point is chosen by watching the database, not by sleeping. At the last
position the leader can still finish before the kill lands; the test then tries
that position again, up to five times, and fails if the kill never lands inside
the run. It is swept across five positions, including
both boundaries: killed after the very first commit, and killed with one
subscription left.

| Killed after | Successor generated | Final invoices | Distinct (sub, period) | Total billed |
|---:|---:|---:|---:|---:|
| 1 | 39 | 40 | 40 | $1,000.00 |
| 5 | 35 | 40 | 40 | $1,000.00 |
| 12 | 28 | 40 | 40 | $1,000.00 |
| 25 | 15 | 40 | 40 | $1,000.00 |
| 39 | 1 | 40 | 40 | $1,000.00 |

**0 duplicate invoices. 0 lost invoices. 0 cents of drift.** The leader's work
and the successor's work are exactly complementary at every kill point.

The test asserts that the process died *of* `SIGKILL` (`WaitStatus.Signal()`)
instead of trusting that it did. An earlier version of this harness had the
helper process panic on its own. A panic also closes the socket, so the test
passed while measuring Go's runtime instead of failover.

## 2. Four leaders racing at once

A crash produces one interleaving. Running four leaders concurrently against
the same due set produces many. The four leaders are four instances of the
billing engine, raced as goroutines in one test process.

These interleavings include both transactions inside the same
`(subscription, period)` window at the same moment. A sequential takeover
cannot construct that state on purpose.

```
4 concurrent leaders: generated=[11 8 12 9]  failures=[0 0 0 0]
final: 40 invoices over 40 distinct (sub,period) tuples, 100000 cents
```

All four leaders did real work (11+8+12+9 = 40), none reported an error, and the
result is still exactly one invoice per subscription.

The test asserts that **at least two leaders generated something**. Without
that assertion, this scenario passes vacuously. With a normal batch size, the
first leader claims every due subscription before the others finish starting.
The others find nothing to do, so a green result means only that three
goroutines arrived late. Forcing one subscription per fetch is what creates the
contention.

## 3. The negative control — what happens without the constraint

The same four-leader run with `idx_invoices_billing_idempotency` dropped:

```
4 concurrent leaders: generated=[26 26 26 25]  failures=[0 0 0 0]
final: 103 invoices over 40 distinct (sub,period) tuples, 257500 cents
```

**103 invoices for 40 periods, and $2,575.00 billed instead of $1,000.00.**
That is a 2.6× overbill, and every leader reported success. A second run of the
same experiment produced 129 invoices and $3,225.00.

Nothing in the application layer noticed: no error, no retry, no log line. On
the code these runs measured, the index was the only guard.

## 4. Money invariants after recovery

Counting invoices proves nothing was double-billed. It says nothing about
whether the crash left some *other* invariant broken. Examples are an orphaned
line item, a ledger entry without its counterpart, or a subscription whose
period bounds no longer agree with its invoices.

`hardcount-doctor` (Hardcount's invariant-checking CLI) sweeps **28 money invariants**
as read-only SQL. After every scenario above:

```
doctor: 28 checks, 0 violations, 0 errors, ~23ms
```

Two notes on that number:

- The sweep runs under an **admin** connection. These checks carry no tenant
  predicate. Under a role scoped by row-level security, they return zero rows
  and report a clean result without having looked at anything.
- One of the 28 checks is new, added by this work.
  `cycle_invoice_unique_per_subscription_period` looks for duplicate cycle
  invoices directly. Before it existed, the doctor ran all 27 checks against a
  database holding 129 invoices where 40 should exist. It reported zero
  violations, because it was blind to the exact failure the index prevents.

The index and the check fail independently. An index can be:

- missing after a restore;
- dropped by a migration;
- left `INVALID` by a failed `CREATE INDEX CONCURRENTLY`, which enforces
  nothing while it still appears in `pg_indexes`;
- quietly narrowed by a predicate change, as this predicate already was once.

## 5. How fast a dead leader is replaced

Since 2026-08-30 (ADR-114), leadership is a per-tick lease row on the database
clock (a [leader lease](../ops/runbook/index.md#words-on-these-pages)). It replaced a session
advisory lock. The lease has a 10 s TTL, is renewed every 3 s, and is polled
at least every 5 s. The old holder stops itself after 6 s without an
acknowledged renew.

Measured on the lease shape (local Docker Postgres, 2026-08-30):

| Failure | Time until a successor holds the role |
|---|---:|
| Process dies, host survives (`SIGKILL` of a mid-tick holder) | **9.52–9.55 s**, 5 of 5 runs (`TestLeaseFailover_SIGKILLedHolderReplacedWithinTTL`, which also asserts no successor takes over while the dead holder's lease could still be live) |
| Host disappears without closing the socket (severed link) | **9 s** (`scripts/partition-drill.sh`, successor acquire confirmed; the holder process was still alive) |

Both failures give the same number because the mechanism is the same. The row
expires `LeaseTTL` after the last renew that landed, whatever the holder's
socket is doing. So the two failure modes of the old mechanism collapse into
one bound, which no longer depends on the network honouring a keepalive. For
the worst-case time to the next tick, add one scheduler poll (≤ 5 s).

The advisory lock this replaced took 95 s to release after a severed link
([ADR-114](../adr/114-leader-leases-tick-scoped-fencing.md); the old runs are
[kept as a record](../dev/records/failure-correctness-advisory-lock-2026-08.md)).

---

## What this does not show

- **This is a correctness result, not a scale result.** 40 subscriptions on one
  laptop. It says nothing about throughput, which is measured in
  [sustained throughput](sustained-throughput.md).
- **Single-node Postgres.** No replication, no failover of the database itself.
  A leader dying is not the same event as Postgres dying, and only the first is
  measured here.
- **Simulated clock.** Both leaders run at one fixed instant so "which periods
  are due" is deterministic. Wall-clock behaviour across a real month boundary
  is covered by other tests, not by this one.
- **No payment provider in the loop.** The charger is a sentinel that fails
  loudly if reached, so this measures invoice generation, not settlement.
- **Not covered:** failover between physical machines.

## Reproducing

```bash
docker compose up -d postgres
go test -p 1 ./internal/billing/ -short=false -run TestExactlyOnce -v
go test -p 1 ./internal/platform/leader/ -short=false -run TestLeaseFailover -v
```

The partition figure (9 s) comes from `scripts/partition-drill.sh` (see [section 5](#5-how-fast-a-dead-leader-is-replaced)).

To reproduce the negative control, drop `idx_invoices_billing_period_dedup`
(`idx_invoices_billing_idempotency` before migration 00215) from the test database and re-run the first command. On code after ADR-115,
dropping the index alone may no longer produce duplicates (see the note at the
top).

**Next:** throughput is measured separately in
[sustained throughput](sustained-throughput.md).
