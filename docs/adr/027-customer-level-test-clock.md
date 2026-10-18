# ADR-027: Test clocks attach at the customer level (Stripe parity)

**Status:** Accepted
**Date:** 2026-05-05

## Summary

Test clocks attach to the customer, as in Stripe: a customer is pinned to a clock when it is created, and the pin never changes. Every subscription of that customer runs on the customer's clock, so credits, payment methods and billing share one "now". `subscriptions.test_clock_id` stays as a copy written at creation, and `GET /v1/test-clocks/{id}/customers` lists a clock's customers. The cascade described below is gone: deleting a clock now deletes its customers and all their rows ([ADR-086](086-simulated-data-lifecycle.md)). Status: active.

## Context

Velox originally attached test clocks at the **subscription** level
(`subscriptions.test_clock_id`). Operators reported two friction
points that surfaced during a single-session UX review:

1. **Asymmetric attach UI.** The Customer Detail page's create-sub
   dialog had the "Pin to test clock" dropdown; the global
   Subscriptions page's create dialog didn't. Same model object,
   two creation surfaces, only one carried the control.
2. **No way to attach a clock to an existing customer.** The pin
   was set at sub creation only; switching required canceling the
   sub and creating a new one.

Behind those friction points sat a deeper design issue. The
per-sub model allowed a single customer to have:
- Sub A pinned to test clock X.
- Sub B pinned to test clock Y.
- Sub C on wall-clock.

Cross-sub state owned by the customer (credit balance, payment-method
setup, billing profile, dunning override) is then consumed by subs
running on different clocks. "Now" becomes ambiguous at the customer
level. Tax-calc timing, credit-grant expiration, dunning-retry
scheduling all assume a single coherent clock per customer; the
per-sub model gave us flexibility we didn't actually want.

A check of the comparable systems we benchmark against:

| System | Attach point | All-or-nothing per customer |
|---|---|---|
| Stripe | Customer (`POST /v1/customers` with `test_clock`) | Yes — once attached, every Sub / Invoice / PaymentIntent for that customer uses the clock |
| Lago | (no test-clock primitive) | n/a |
| Recurly | Customer | Yes |
| Velox (pre-fix) | Subscription | No |

Stripe's all-or-nothing constraint is more restrictive and that's
the right tradeoff: simulation time consistency for a single
customer is load-bearing for billing correctness, and the
flexibility-cost of being unable to mix clocks under one customer is
purely theoretical (no operator has asked for it).

## Decision

Test clocks attach at the **customer** level. Subscriptions inherit
their clock from the owning customer. Stripe parity end-to-end.

### Schema

`customers.test_clock_id TEXT REFERENCES test_clocks(id) ON DELETE SET NULL` —
new column, FK with the same shape as the existing
`subscriptions.test_clock_id`. Migration 0078 backfills it from
existing per-sub pinning (DISTINCT ON customer, ordered by oldest
sub first) and reconciles any sub whose clock disagrees with the
customer's chosen clock.

`subscriptions.test_clock_id` stays — as a denormalized cache. The
billing engine continues reading it directly (no JOIN added);
writers ensure `sub.test_clock_id == customer.test_clock_id` at
sub-create time. Decoupling deferred to a future migration once all
read paths are confirmed customer-driven.

### API

`POST /v1/customers` accepts `test_clock_id`:
- Test-mode keys only (live mode rejects with 400).
- Validates the clock exists for the tenant (404 if not).
- Once set, the customer's `test_clock_id` is **immutable** —
  `PATCH /v1/customers/:id` does not touch the column. To switch
  clocks: delete + recreate the customer (Stripe parity).

`POST /v1/subscriptions` accepts `test_clock_id` for backward
compatibility but the value is reconciled against the customer's:
- Customer pinned + sub input matches → OK, write through.
- Customer pinned + sub input empty → inherit from customer.
- Customer pinned + sub input differs → 400.
- Customer unpinned + sub input set → 400 ("customer is not
  attached to a test clock").

`GET /v1/test-clocks/:id/customers` — new endpoint. Returns
customers attached to a clock. Powers the "Attached customers"
surface on the test-clock detail page (Stripe parity).

### UI

- **Customers page Create Customer dialog**: gains "Pin to test
  clock" dropdown, gated on test mode + clocks-exist. Sets
  `test_clock_id` at create time.
- **Customer Detail header**: shows test-clock badge when the
  customer is pinned. Operator immediately sees that everything
  for this customer is on simulated time.
- **Customer Detail Create Subscription dialog**: drops the
  per-sub dropdown. Replaced with a read-only inheritance hint
  ("This subscription will inherit the customer's test clock —
  Demo Clock") when applicable. Subs follow the customer.
- **Test Clock Detail page**: gains "Attached customers" section
  above the existing "Subscriptions on this clock" list. Customers
  are the primary surface (Stripe parity); subs remain visible for
  drill-down.
- **Subscriptions page Create dialog**: no longer needs the
  dropdown. Asymmetry resolved structurally.

### Inheritance enforcement

Service-layer:
- `customer.Service.Create` validates the clock exists.
- `subscription.Service.Create` reads the customer's clock and
  reconciles input.

DB-layer:
- No CHECK constraint or trigger (CHECK can't reference another
  table; trigger overhead not justified pre-launch). Application
  is the single writer; tests cover the invariant.

### Cascade

When a clock is soft-deleted (ADR-016): cascade-cancels pinned subs
by `subscriptions.test_clock_id`, **and (added 2026-06-13) detaches
pinned customers** (`customers.test_clock_id → NULL`) in the same
transaction.

The customer detach closes a stranding gap this ADR introduced: by
moving the pin to the customer (immutable, create-only, inherited by
new subs), a customer left pinned to a soft-deleted clock would have
its next subscription inherit the dead clock and never bill (excluded
from the wall-clock cron — it's pinned — and from the catchup path —
the clock is deleted). The `customers.test_clock_id` FK declares
`ON DELETE SET NULL`, but ADR-016's soft-delete means the row is
never DELETEd, so that cascade never fired — the detach realizes it
in application code. See ADR-016 ("Customer detach"). Going-forward
only; no data-repair migration ships (pre-launch — any rows already
broken are corrected directly in the local DB).

Consequence: the clock-detail-page "attached customers" surface goes
empty for a deleted clock (its customers are no longer pinned to it).
That surface was only reachable for a soft-deleted clock by id anyway
(the live filter hides it from the list); "which customers ran on
this clock" stays answerable through the canceled subs' retained
`test_clock_id` pointers.

## Consequences

- **Stripe API parity for test clocks.** Operators migrating from
  Stripe see the same primitives in the same places.
- **No more cross-sub time inconsistency.** A customer's credit
  ledger, payment-method state, and billing profile all share one
  "now."
- **Removed friction**: pin is on the customer — operators don't
  hunt across sub-create dialogs, and the asymmetric Subscriptions-
  page dialog UX gap is structurally gone.
- **Schema bloat is small**: one nullable column + one index.
- **Migration is data-lossy under one specific scenario**: an
  existing customer with subs on multiple clocks gets reconciled
  to one (oldest-sub's clock wins). Acceptable pre-launch; for any
  future production migration this would be opted into separately.

## Alternatives considered

- **Tier 1 (mirror the dropdown to Subscriptions page).** Closes
  the visible asymmetry but bakes in the wrong design — every
  future test-clock feature inherits per-sub semantics. Rejected
  as a band-aid.
- **Per-sub with DB-level invariant trigger** ensuring sub.clock ==
  customer.clock. The trigger overhead and the operational
  complexity of cross-table CHECK isn't justified when the
  application is the single writer.
- **Drop `subscriptions.test_clock_id` entirely** in this migration.
  Touched too many engine queries (ListSubscriptionsOnClock,
  cascade-cancel, billing engine's `effectiveNow`); deferring to a
  future cleanup once all read paths are confirmed customer-
  driven via a downstream migration.
- **Allow updating customer.test_clock_id post-creation.** Stripe
  doesn't; the constraint is the point. A customer's "now" should
  be stable across the customer's lifetime; switching mid-lifecycle
  would break invoice timestamps, dunning timestamps, etc.

## Tests

- Migration backfill: customer with pre-existing per-sub pinning
  ends up with the right clock; customers with no subs unchanged.
- Migration reconcile: subs whose clock differs from the customer's
  get realigned.
- `customer.Service.Create`: rejects unknown `test_clock_id`,
  rejects when checker not wired, accepts valid clock.
- `subscription.Service.Create`: rejects mismatched
  `test_clock_id`, inherits when input empty, allows match.

## What ships next (deferred)

- Drop `subscriptions.test_clock_id` once all read paths confirmed
  customer-driven (a separate migration; no rush).
- API field `test_clock_id` deprecation note on
  `POST /v1/subscriptions` (kept for back-compat per ADR-027 above).
- UI parity gap for SubscriptionDetail page: show inherited-clock
  badge using the customer's clock, not the sub's column. Cosmetic;
  defer.
