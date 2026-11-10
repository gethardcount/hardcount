# ADR-139: A cancel prices before its transaction, then proves the plan on it

**Date:** 2026-11-10
**Status:** Accepted

## Summary

An immediate cancel, a scheduled mid-period cancel and dunning's cancel now price their final invoice and plan their relief with no transaction open: the usage reads, the tax call (a network call for a Stripe Tax tenant) and the reads of the funding invoices all run first. Then one transaction writes the plan. Its first statement is a compare-and-set on the period the plan was priced from. Next it compares the items and the billing-interval rows with the ones planned, and reads the threshold watermark again. If anything moved, nothing is written and the cancel plans again once; a second move is returned as `ErrWatermarkMoved`, which the cancel API answers `409` `concurrent_change`. Before, the pricing ran inside the transaction, so a slow tax call held the subscription row and a pool connection for its whole length. The cross-interval plan swap still prices inside its transaction (P1-23 PR-2).

## Context

- **The stall.** The cancel's transaction locked the subscription row with its first statement (`UPDATE subscriptions SET status = 'canceled'`), then priced the final invoice. The pricing read plans, settings, intervals, meters, floors and usage on other pool connections, and called `provider.Calculate`. The Stripe client allows 30 seconds, with 2 retries. For that whole time the row, the sealed interval rows and the threshold invoice (`FOR KEY SHARE`) were held.
- **The pool.** `DB_MAX_OPEN_CONNS` is 20 by default. Each cancel held one connection in its transaction and asked for a second one to price. With about 20 cancels at once, every connection was held by a transaction waiting for another connection: none could proceed until its request timed out, and every other request and background job waited too.
- **The session kill.** Postgres must kill a session idle in a transaction for 30 seconds ([Postgres requirements](../ops/postgres-requirements.md)). A tax call that long killed the cancel's session, and a scheduled cancel recorded a counted billing failure.
- **The target shape exists.** The cycle close already prices before its transaction and proves its snapshot as the first statement (`commitPeriodClose`, [ADR-115](115-one-closer-for-the-billing-period.md)).

## Decision

1. **Plan.** `billing.Engine.PlanCancelBill(ctx, sub, at)` prices `sub` as canceled at `at`: the guards, the threshold watermark (read on its own connection), the lines, the tax call. Then, last, it plans the relief (`planRelief`): which funding invoices to void and the credit-note share of each other one, with each invoice's room read on its own connection. It writes nothing and returns a `subscription.BillWriter`.
2. **Prove.** `subscription.Service` opens the transaction through the store (`CancelAtomicWithBill`, `FireScheduledCancellationWithBill`), which take the planned `PeriodSnapshot` and `at`:

   | Read before the transaction | Proof inside it |
   |---|---|
   | Status, period start, `next_billing_at` (and `cancel_at` for the scheduled cancel) | The first statement is the compare-and-set: `UPDATE … WHERE status … AND current_billing_period_start IS NOT DISTINCT FROM … AND next_billing_at IS NOT DISTINCT FROM …`. Zero rows resolve to not found, already terminated, superseded (scheduled), or `ErrWatermarkMoved`. The status alone catches a trial the activation scan converted after the cancel read the row and before it read the billing intervals: the conversion moves no period column and no item |
   | Items (id, plan, quantity) | `sameItems` on the items read on the transaction. Every item writer locks the row first |
   | Billing intervals | `ListItemIntervalsTx` on the transaction must give the same rows on item, plan, quantity and start (`sameIntervals`). It catches an item added then removed, and a change made and undone, which `sameItems` misses. `ends_at` is left out: the cancel's own seal writes it |
   | Threshold watermark | Read again on the transaction `FOR KEY SHARE` and compared with the planned instant (`writePlanned`), as the cycle close does |
   | Collection pause | Not proved. The final invoice takes its status from the locked row |
   | Funding invoices, credit-note room | The existing checks: `VoidUnchangedTx` refuses a changed invoice (`invoice_changed`), and each draft re-checks its room under the invoice's credit-note lock |
   | Usage, plans, prices, settings, billing profile | None, as for the cycle close |

3. **Write.** On the same transaction, `writePlanned` writes the relief (voids, then the credit-note locks of every invoice it credits, then the drafts), then numbers and inserts the final invoice, its finalize audit row and its usage-below-zero row; the service writes the cancel's audit row; the store retires the row and queues `subscription.canceled`. Nothing in the transaction reads on another connection or calls a provider: credit-note create reads its invoice on the caller's transaction (`invoice.GetTx`, `ListLineItemsTx`), and the void's credit return binds no clock.
4. **The instant `at`.** Each attempt of the immediate cancel binds its instant from the caller's context: on the API's unbound context the second attempt reads a new wall-clock instant; dunning's context arrives bound, so both attempts use dunning's instant, as do both on a test clock. An active row whose period starts after `at` is not planned: priced from it, the final invoice bills nothing and the relief skips the new period's prebill, while the compare-and-set passes. The attempt answers `ErrWatermarkMoved`. A trialing or draft row is planned (a cancel before the first paid period is the free trial cancel, [ADR-069](069-trial-cancel-semantics.md)). The scheduled cancel's `at` is its `cancel_at`, the contracted instant, and it is not checked against the period start ([ADR-097](097-mid-period-scheduled-cancel-fires-as-immediate-cancel.md)).
5. **Planning again.** The immediate cancel runs at most two attempts. After the second `ErrWatermarkMoved` it returns the error: the cancel handler answers `409` `concurrent_change` (not stored by the idempotency layer); dunning labels its run `action_failed` and runs it again 24 hours later. The scheduled cancel returns `ErrWatermarkMoved` to `billSubscription`, which reads the row again and fires again once, as it does for a moved period close; a second miss leaves the subscription due, and the drain counts it (`errStillDue`).

## Why this design

- **The lock is held only while writing.** Nothing slower than a statement runs between the first statement and the commit. A cancel holds one connection, briefly; the tax call holds none.
- **One rule for every period closer.** The cancel proves its snapshot exactly as the cycle close does, so a reader learns one mechanism.
- **A moved snapshot is not a failure.** A race that used to wait on the row lock now plans again, so dunning and the billing run do not record a failure for it.

## Alternatives considered

- **A. No second plan.** A moved snapshot always answers `409` or returns the error. About 40 lines less, but dunning's cancel would wait 24 hours, and a scheduled cancel would count a billing failure, for a race that today only waits on the row lock.
- **B. The engine owns the transaction**, as `commitPeriodClose` does. The cancel's audit row, the `canceled_by` provenance and the swap's item write would move behind engine callbacks. About twice the change, with no lock or pool gain over the chosen shape.

## Consequences

### Positive
- A slow Stripe Tax call no longer blocks other writers of the subscription, and no number of concurrent cancels can hold every connection.
- The 30-second idle-in-transaction kill no longer applies to the tax call.
- An immediate cancel that read a row whose period started after its instant no longer cancels with the new period unrelieved: it plans from the new period, or answers the error.

### Risks / open items
- **A discarded plan costs one tax calculation.** It is paid for and never committed, so no liability is reported. Its `tax_calculations` row stays with no invoice; nothing reads unlinked rows, and a cancel that rolls back after pricing left the same row before. The scope key carries `canceled_at`, so the API cancel's second plan is a second calculation; dunning's, a test clock's and the scheduled cancel's reuse the first while the lines are unchanged.
- **A relief refusal still comes after the tax call**, as since P1-08: `funding_invoice_tax_pending` and `invoice_changed` leave one discarded calculation.
- **Dunning's time budget.** Dunning's cancel runs under a 5-minute charge hold. Two plans during a Stripe Tax brownout can outlast it; the relief's void then refuses, the cancel rolls back, and the run is labelled `action_failed` and runs again after 24 hours. Nothing is lost.
- **The cross-interval plan swap is unchanged** until P1-23 PR-2: it still prices and plans its relief inside its transaction (`BillOnPlanSwapDraftsTx` plans and writes in one call).
- **The teardown deadlock** ([ADR-136](136-deadlock-loser-answers-409-at-its-handler.md), D2) stays possible at the watermark re-read; the cancel handler still converts it.

## References

- [ADR-056](056-atomic-cross-interval-plan-swap.md), [ADR-115](115-one-closer-for-the-billing-period.md), [ADR-118](118-cancel-and-swap-relief-in-the-change-transaction.md), [ADR-066](066-threshold-money-semantics.md), [ADR-097](097-mid-period-scheduled-cancel-fires-as-immediate-cancel.md), [ADR-136](136-deadlock-loser-answers-409-at-its-handler.md)
- `internal/billing/engine.go`: `PlanCancelBill`, `writePlanned`, `planRelief`, `writeReliefTx`
- `internal/subscription/service.go`: `cancel`, `cancelOnce`, `FireDueScheduledCancel`, `sameIntervals`
- `internal/subscription/postgres.go`: `CancelAtomicWithBill`, `FireScheduledCancellationWithBill`
- Tests: `internal/billing/cancel_plans_before_tx_integration_test.go`, `internal/api/cancel_replan_e2e_test.go`
