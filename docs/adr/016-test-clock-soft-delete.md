# ADR-016: Test clocks are soft-deleted with cascade-cancel of pinned subs

**Status:** SUPERSEDED by [ADR-086](086-simulated-data-lifecycle.md) (2026-07-09) — clock-delete becomes a hard `DELETE` with an FK cascade keyed on `sim_clock_id` (teardown of the whole simulated sandbox, customer included), replacing the soft-delete + cancel-subs + detach-customers design below. The soft-delete's residual leakage (a detached customer's stranded simulated rows swept against wall-clock) is exactly what ADR-086 dissolves.
**Date:** 2026-05-04

## Summary

Superseded by [ADR-086](086-simulated-data-lifecycle.md) on 2026-07-09. This ADR made deleting a test clock a soft delete: set `deleted_at`, cancel the clock's subscriptions and, from 2026-06-13, detach its customers. That left simulated rows behind for real-time jobs to pick up. Deleting a clock now hard-deletes the clock, its customers and every row they own in one transaction, and keeps only the audit log.

## Context

Velox's other entities are uniformly soft-deleted: tenants, customers,
plans, subscriptions, and credit notes use a `status` column with
terminal states; coupons + API keys use timestamp columns
(`archived_at`, `revoked_at`). Invoices are immutable in their
terminal states (paid, voided). Live financial data never silently
vanishes — audit trail integrity is one of Velox's load-bearing
invariants.

`test_clocks` was the lone exception: hard `DELETE FROM test_clocks`
plus an `ON DELETE SET NULL` on `subscriptions.test_clock_id`. This
produced two problems an operator surfaced on 2026-05-04:

1. **Silent orphan subs.** Subscriptions detached when their clock
   was deleted kept `next_billing_at` set in simulation time — values
   the wall-clock scheduler couldn't reconcile (sometimes future
   absolute dates, sometimes long-past). The subs sat dormant until
   either operator cleanup or a misfire whenever the wall clock
   eventually crossed those dates.

2. **Broken audit refs.** `audit_log` is append-only with a trigger;
   entries that referenced the deleted clock by id became
   unresolvable. For test-mode entities this is cosmetic, but it's
   inconsistent with how every other entity in Velox preserves audit
   integrity.

The dialog copy on the operator-facing delete confirmation also
*lied* ("This deletes the clock and any subscriptions pinned to it"
— the SQL only set their `test_clock_id` to NULL).

## Decision

`test_clocks` gets a `deleted_at TIMESTAMPTZ NULL` column (migration
0073) and joins the rest of Velox's soft-delete convention. The
service-layer `Delete` operation:

1. `UPDATE test_clocks SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`
2. `UPDATE subscriptions SET status = 'canceled' WHERE test_clock_id = $1 AND status NOT IN ('canceled', 'archived')`
3. `UPDATE customers SET test_clock_id = NULL WHERE test_clock_id = $1` (added 2026-06-13 — see below)
4. All in the same transaction.

All read paths (`Get`, `List`, `ListAllAdvancing`, the status
transition CAS) filter `WHERE deleted_at IS NULL` so a soft-deleted
clock disappears from the operator surface. The status-transition
filter prevents an in-flight catchup worker from advancing a clock
that's been soft-deleted while it was running.

Generated invoices are intentionally untouched. Velox's invoice
immutability rule (terminal-state finalized/paid/voided rows never
mutate) takes precedence; the simulated timestamps on those invoices
remain self-evident from the now-deleted clock, and any future audit
query can still resolve them by id.

`test_clock_id` is intentionally NOT set to NULL on cascade-canceled
subs — keeping the historical pointer lets future tooling answer
"which clock did this sub belong to" without a separate audit query.
Subs are safe to keep pinned because cancellation is terminal: the
stale simulation-time `next_billing_at` they carry never fires.

### Customer detach (added 2026-06-13)

Step 3 above detaches pinned **customers** (`test_clock_id → NULL`).
This was a gap, not part of the original ADR: when ADR-027 moved the
pin to the customer level the day after this ADR shipped, the
customer kept pointing at the soft-deleted clock — and because the
FK's `ON DELETE SET NULL` only fires on a row DELETE (never on our
soft `deleted_at`), nothing cleared it. A new subscription for that
customer then **inherited the dead clock** (ADR-027 customer-level
inheritance) and landed stranded: the wall-clock cron skips it (it's
pinned) and the catchup path skips it (the clock is deleted), so it
never billed. Unlike a sub, a customer has no period fields to go
stale, so detaching it is safe — its next subscription is a clean,
billable wall-clock sub. "Which customers were on this clock" stays
answerable through the canceled subs' retained pointers. This is a
going-forward fix only; no data-repair migration ships (pre-launch —
any rows already broken are corrected directly in the local DB).

### TTL sweeper (reverted 2026-05-06)

The schema carried `deletes_after TIMESTAMPTZ` since migration 0020
as a Stripe-parity 30-day idle cleanup hook. This ADR originally
completed the consumer side (`Store.SweepDueDeletes` running in the
scheduler tick, soft-deleting any clock whose `deletes_after` had
elapsed and cascade-cancelling its pinned subs).

The producer side never landed: nothing in Velox ever wrote
`deletes_after` on a clock — not the create endpoint, not the SPA,
not a background job. The sweeper matched zero rows on every tick,
and the only thing that ever set a value was the manual psql step
in MANUAL_TEST FLOW TC2 — testing the sweeper machinery itself, not
a feature any operator triggered.

Migration 0080 (2026-05-06) drops the `deletes_after` column, the
partial index, the `SweepDueDeletes` store + service methods, the
`TestClockSweeper` scheduler interface and the corresponding
scheduler step. The MANUAL_TEST step that exercised it is removed.
Soft-delete + cascade-cancel of pinned subs survives unchanged on
the operator-driven `Delete` path. If a customer later asks for
auto-cleanup of idle clocks, wiring it back is a small ADR — write
a creation-time TTL default + restore the sweeper.

### Why timestamp + not status

Every existing `test_clocks.status` value (`ready`, `advancing`,
`internal_failure`) describes a live operational state, not a
soft-delete marker. Adding a `deleted` status would force an awkward
"clocks in advancing state when deleted" question; a separate
timestamp column captures lifecycle independently of operational
state, mirrors `coupons.archived_at` + `api_keys.revoked_at`, and
lets us tell "deleted while advancing" apart from "deleted while
ready" if forensics ever need it.

### Why not match Stripe's hard-delete-with-cascade

Stripe Test Clocks hard-delete the clock + every customer + sub +
invoice created against it (clocks own a sandbox of resources).
That works for Stripe's product because:

- Stripe has separate test-mode infrastructure
- Stripe's test data has its own audit retention policy
- Stripe explicitly documents test data as ephemeral

Velox runs the same code path against test and live mode through a
single audit_log + RLS partition. Hard-deleting test entities would
diverge from Velox's everywhere-else soft-delete convention and
create a special case for test-mode audit refs. Soft-delete with
cascade-cancel keeps the model uniform; the operator outcome is
nearly identical (the clock and its pinned subs are gone from every
operator surface).

## Consequences

### Positive

- Internal consistency: `test_clocks` matches the rest of Velox.
- Audit integrity: no orphan refs from `audit_log`.
- No silent orphan subs: pinned subs land in the canceled-subs view,
  visible to the operator instead of dormant in unbillable state.
- TTL sweeper completes a half-built design (`deletes_after` column
  has had no sweeper since 0020).
- Reversibility: a wrong-clock-deleted is recoverable via SQL UPDATE
  if needed (set `deleted_at = NULL`, undo the sub cancellations).

### Negative

- Soft-deleted clocks accumulate in `test_clocks` until the TTL
  sweeper runs. The partial index on `(tenant_id, created_at DESC)
  WHERE deleted_at IS NULL` keeps live queries cheap regardless.
- Idempotent re-delete returns `ErrNotFound` (the live filter hides
  the row). A caller that wanted "delete or no-op" semantics now has
  to ignore that error explicitly. Acceptable trade-off — the
  current single caller (the dashboard delete dialog) treats it as
  success either way.

## Compatibility

- API surface unchanged: `DELETE /v1/test-clocks/{id}` returns 204
  on success, 404 on already-deleted.
- Frontend dashboard surface unchanged for live operator views;
  soft-deleted clocks disappear from the list as before.
- Dialog copy updated to truthfully describe the cascade behavior:
  "This removes the clock and cancels its N pinned subscriptions."

## Notes

- `ON DELETE SET NULL` on the FK is preserved as a backstop for any
  future operator-driven hard delete (out-of-band DBA cleanup) so
  the FK side still degrades gracefully. The service never takes
  that path.
- Tests in `internal/testclock/postgres_test.go` cover both the
  cascade-cancel-on-delete and TTL-sweep-on-due paths against real
  Postgres.
