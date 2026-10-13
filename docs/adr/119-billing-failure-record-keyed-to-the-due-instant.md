# ADR-119: A billing failure is saved on the subscription, keyed to the due instant

**Date:** 2026-10-12
**Status:** Accepted

## Summary

When the billing run fails to bill a subscription, the failure is now saved on
the subscription row. The next attempts wait on a fixed ladder, and after eight
failed attempts the subscription is parked until an operator retries it.

The saved record is tied to the piece of work that failed: it stores the
instant that work became due, and it counts only while the subscription is
still due at that same instant. When the work is done or goes away, the due
instant changes and the record stops counting. Nothing has to clear it.

An interrupted run or a lost database connection is not counted and saves
nothing. `POST /v1/subscriptions/{id}/retry-billing` clears the record, writes
an audit row, and bills at once. Its 200 response says which of three things
happened: billed, failed again, or not completed and still due.

## Context

Issue #960. Before this change a subscription whose billing failed was
attempted again on every billing run, for as long as it kept failing. The only
trace was one `bill subscription failed` ERROR line per attempt.

That had three costs:

- **Nobody could see it.** No API field, list or count showed that an invoice
  was not being issued. An operator had to read logs.
- **It never stopped.** A subscription that could not bill was attempted every
  hour, without limit.
- **Leadership changes replayed it.** Each new leader of the billing role
  attempted every failing subscription at once. A subscription whose billing
  panicked took down every tick, on every replica that led next.

Terms used below:

- **Due instant:** the instant a subscription next owes work to the billing
  run. It is the earlier of `next_billing_at` (a period close) and, on an
  active subscription, `cancel_at` (a scheduled mid-period cancel). The due
  scans already test exactly this.
- **Parked:** automatic attempts have stopped. The subscription stays due, and
  only an operator's retry or a change to the due work restarts it.
- **Wall-clock subscription:** one not attached to a test clock.

## Decision

**Schema (migration 0176).** Five columns on `subscriptions`:
`billing_failed_for`, `billing_failure_count`, `billing_failed_at`,
`billing_next_attempt_at` and `billing_last_error`. There is no new table,
trigger, CHECK constraint, index or `status` value. `next_billing_at` is never
moved to implement a wait.

**The key.** A record is live only while

```sql
status IN ('active','trialing')
AND test_clock_id IS NULL
AND billing_failed_for IS NOT NULL
AND billing_failed_for IS NOT DISTINCT FROM
    LEAST(next_billing_at, CASE WHEN status = 'active' THEN cancel_at END)
```

This predicate lives in one place, `domain.SubscriptionBillingFailingSQL`. Its
Go form is in the subscription store's row scan, which sets
`Subscription.BillingFailure`. Every reader uses one of the two, with one
exception: the gauges in `internal/api/middleware/metrics.go` carry a hand copy
of the SQL, because that package imports no domain package.
`TestBillingGauges_DueAndFailingPartition` pins the copy to the predicate on
real rows; change both together.

**States.** They are derived, not stored:

| State | Meaning |
|---|---|
| Healthy | No live record. |
| Retrying | Live record with `billing_next_attempt_at` set. |
| Parked | Live record with `billing_next_attempt_at` NULL. |

**Two writers, and only two.** Both write only the five columns.

- `RecordBillingFailure` runs after a failed attempt, in its own transaction.
  Its `WHERE` requires that the row's due instant still equals the one the
  attempt read, and that the count the row holds still equals the count the
  attempt read.
- `ClearBillingFailure` runs for the operator retry, in the same transaction
  as the audit row.

**The gate.** Three wall-clock scans carry it: the scheduler's due scan, the
scan behind `POST /v1/billing/run`, and the trial-expiry scan. Each skips a
subscription whose live record is parked, or whose next attempt is after the
run's instant. `billSubscription` checks again after its own read of the row.
The trial-expiry scan gates only the activation of a trial. A trialing
subscription with a cancel due at trial end is always returned, so a cancel
scheduled on a parked trialing subscription still fires.

**What counts.** `classifyBillFailure` reads the run's context and the typed
database error. It never matches message text.

| Class | When | Saved? |
|---|---|---|
| Interrupted | The run's own context is done: shutdown, a lost lease, a request timeout. | No |
| Transient | The connection was lost (including a network timeout with no deadline behind it), the server is shutting down, or the server aborted the transaction for a serialization conflict or a deadlock. | No |
| Counted | Everything else, including every timeout while the run is alive, a panic, and an attempt that returns cleanly but leaves the subscription due. | Yes |

**The operator retry.** `POST /v1/subscriptions/{id}/retry-billing` clears the
record and writes the audit row in one transaction, then bills inline through
the same attempt as the billing run. After the clear has committed, the
attempt's outcome is never an HTTP error, because a caller that repeated the
call after a 5xx would get 409 for a subscription that is still unbilled. The
200 response has three outcomes:

| Outcome | `billing_failure` | `still_due` |
|---|---|---|
| Billed, or nothing was left to bill | absent | false |
| Failed again and saved, at attempt 1 | present | false |
| Not completed, nothing saved (lost connection, deadlock, failed failure-write) | absent | true |

An attempt that returns cleanly but leaves the subscription due is the second
outcome. The retry re-reads after its attempt and saves "no progress". The
billing run does the same when the subscription comes back on a later page.

The classes are decided by error type only. Two database faults therefore
count although the subscription did not cause them: a statement or lock
timeout during an overload, and a server that refuses writes during a failover
(SQLSTATE 25006, a read-only standby). The first cannot be separated from a
query that is too slow for its own data. The second is left counted until a
mixed pool is seen in practice.

A failure on a test-clock subscription is classified the same way and is never
saved. Its log line reads `class=counted recorded=false
not_recorded_reason=test_clock`.

**The ladder.** After counted failure 1 the wait is 15 minutes, then 1 hour,
4 hours, 12 hours, and 24 hours three times. The eighth failure parks. The
earliest park is 3 days 17 hours 15 minutes after the first failure. The
numbers are constants in Go (`domain.MaxBillingAttempts`,
`billing.billRetryWaits`) and are not configurable.

**The saved error text is an allowlist.** A curated sentence is saved as
written. Any other error is saved as "An internal error occurred. Reference:
<subscription id>", and the full error stays in the log under that id.

**A panic** while billing one subscription is saved as a counted failure and
then raised again, so the tick still fails loudly. It panics again on each
later attempt, so one such subscription ends at most eight ticks early before
it parks.

**Test clocks** are unchanged. A subscription on a test clock never gets a
record, and the test-clock scans carry no gate.

## Why this design

**The key replaces every clear.** Fifteen statements update a subscription
row. Any of them could make failing work stop being due: a clean period close,
a plan swap, ending a trial, un-scheduling a cancel. A design that clears the
record must get all fifteen right, and stay right when a sixteenth is added.
With the key, the statement that moves the due instant voids the record by
doing so. A writer that knows nothing about billing failures is still correct.

**A failure cannot land on work that never failed.** The failure write proves
the due instant it read. If another run billed the period in the meantime, the
write matches no row. So a record can only ever delay the work it was saved
for, and a gate on that record can never skip a period.

**Money is untouched.** Neither writer changes status, the period,
`next_billing_at` or `cancel_at`. Exactly-once billing still rests on the
period compare-and-swap of ADR-115. The operator retry bills through the same
attempt as the scheduler, so a retry that races a billing run produces one
invoice.

**Choices a later reader might want to undo, and why they stand:**

- **Uncounted failures are not saved.** A saved-but-uncounted state needs a
  record with a count of zero and a second meaning for the next-attempt
  column. Those subscriptions lose nothing by staying unsaved. They are
  attempted on every run and stay visible in the due gauges, as before.
- **Timeouts count.** Velox sets no statement timeout of its own. A timeout is
  the only way a subscription whose own query is too slow ever shows up.
  Treating it as transient would retry that subscription forever, which is the
  original problem.
- **The saved text is an allowlist.** The test-clock failure reason passes any
  error text that has no internal marker. This text is different: it is
  returned on every subscription read, so only curated sentences reach it.
- **An operator retry resets the count.** A tax retry keeps its count. Here the
  operator has just said the cause is fixed, and repeated clicks must never
  park a subscription.
- **`POST /v1/billing/run` honours the gate.** One rule for every run means a
  scripted run cannot use up a subscription's attempts. It is not the way to
  retry one subscription.
- **The bound lives in Go only.** No SQL contains the number 8, so changing it
  never re-reads saved rows.

## Alternatives considered

- **A. Clear the record in each period writer.** Four plain columns, cleared
  by every statement that moves the period. Rejected: review found paths that
  left a park standing after the work was gone (extend trial, end trial,
  un-schedule a cancel), and each new writer is a new chance to forget.
- **B. Key the record on `next_billing_at` alone.** Rejected: a failing
  mid-period cancel has `cancel_at` as its due instant. A record keyed on
  `next_billing_at` would survive un-scheduling that cancel and then block the
  next period close.
- **C. A database trigger that clears the record when the period moves.**
  Rejected: the trigger makes a state change the Go code and the in-memory test
  store cannot see, and it clears the record on unrelated updates without an
  audit row.
- **D. Push `next_billing_at` forward to implement the wait.** Rejected:
  `next_billing_at` is the compare-and-swap watermark, the instant stamped on
  the invoice, and a consistency check in velox-doctor.
- **E. Reuse the tax-retry ladder.** Rejected: tuning tax policy would then
  change time-to-park for billing, and no billing test would notice.

## Consequences

### Positive

- A failing subscription is visible: `billing_failure` on every subscription
  read, `GET /v1/subscriptions?billing=failing`, and the
  `velox_billing_failing_subscriptions{mode,state}` gauge.
- A failing subscription stops being attempted every tick, and stops
  altogether after eight failures.
- A leadership change no longer re-attempts every failing subscription.
- One subscription that panics no longer blocks every later tick.
- Every owed wall-clock subscription is in exactly one of two gauges: due and
  not failing, or failing.

### Risks / open items

- **Cancelling an overdue subscription dropped its ended period. Fixed
  2026-10-13 (#991).** An immediate cancel after the period end wrote no
  final invoice, and no billing run bills a canceled subscription. The
  disposition has two parts:
  - The cancel first bills what is due, through the same attempt as the
    operator retry (`BillDueNow`): one invoice per ended period, the period
    CAS, and a counted failure if it fails. It does this only for an active
    subscription that is due. Then the cancel runs as before, inside the
    current period.
  - The cancel's final-invoice step refuses a cancel whose `canceled_at` is at
    or after the period end: 409 with code `unbilled_period`, and the cancel
    rolls back. This is what holds for a subscription that is waiting or
    parked, because the attempt above does nothing for it. The operator fixes
    the cause, calls retry-billing, and cancels again.

  Three consequences were accepted:
  - A subscription whose billing cannot be fixed cannot be cancelled. A
    "cancel and write off the ended period" action is not built; add it when
    a real stuck subscription needs it.
  - Dunning's final cancel on a failing subscription is refused and tried
    again on each dunning pass until billing recovers. When the wait has
    passed, its attempt is a real one and counts toward the park.
  - On a plan billed in advance the late cancel charges the next period's
    base and then credits the unused part back, as a billing run followed by
    a cancel always did. The credit step no longer voids a renewal invoice
    that also carries the ended period's usage.

  Two sibling paths that move the period of a possibly overdue subscription
  were not traced and are not changed here: the immediate cross-interval plan
  swap and the threshold fire that resets the cycle.
- **A fault that fails every subscription for more than about 3.7 days parks
  them all.** Un-parking is one call per subscription. There is no bulk retry.
  The runbook has the loop, and a page-level alert on the gauge fires long
  before the first park.
- **A timeout uses up attempts.** With a role-level `statement_timeout` set, a
  database overload can leave a wait of up to 24 hours after recovery, unless
  an operator retries.
- **A recurring connection error or deadlock on one subscription never
  parks.** It is not saved either. It shows as a small, flat oldest-due age,
  with the id in the log.
- **Late invoices have existing side effects** that a wait can widen. Prepaid
  credits that expired in the meantime do not apply. An in-advance item
  changed while the period is overdue is billed from the boundary.
- **The five columns go stale.** They are not cleared when the due instant
  moves. An architecture test (`internal/arch`) fails the build if a new file
  reads them without the predicate, and the column comment warns hand-written
  SQL.
- **A failure that kills the process is not recorded,** because the record is
  written after the attempt returns.
- **A retry bills inline** under the request timeout. A subscription overdue
  for many periods may not finish in one request; the next billing run finishes
  it.
- **`POST /v1/billing/run` behaves differently for a caller that scripts it:**
  it now skips waiting and parked subscriptions and returns an empty error
  list for them.
- **Not built:** a notification when a subscription parks, a bulk retry, an
  action that closes a period without an invoice, and the dashboard surfaces
  (a separate change).

## References

- Issue #960.
- [ADR-006](006-background-scheduler-vs-message-queue.md): the billing run and
  its drain; amended by this ADR.
- [ADR-028](028-billing-engine-period-loop-and-disjoint-flows.md): test-clock and wall-clock
  billing are disjoint flows.
- [ADR-090](090-audit-in-tx-emission.md): an audit row shares a transaction
  with the change it records.
- [ADR-114](114-leader-leases-tick-scoped-fencing.md) and
  [ADR-115](115-one-closer-for-the-billing-period.md): the leader lease and the period
  compare-and-swap that exactly-once billing rests on.
- Migration `0176_subscription_billing_failure`.
- Runbook: [14. Subscriptions whose billing is failing](../ops/runbook.md#14-subscriptions-whose-billing-is-failing).
