# ADR-136: A deadlock loser answers 409 at its handler; no in-process retry

**Date:** 2026-11-09
**Status:** Accepted

## Summary

Postgres breaks a deadlock by aborting one of the transactions in it, with SQLSTATE 40P01. Six handlers now answer that abort with `409` `concurrent_change` instead of `500`. The idempotency layer does not store a `409`, so the client sends the same request again with the same `Idempotency-Key`. Before, the `500` was stored, and the key replayed it for 24 hours. Hardcount does not run the transaction again itself, and no lock order changes. `respond.DeadlockAsConflict` does the conversion, only in a handler, and only where what the request saved before the transaction that lost is safe to run twice.

## Context

- **The bug.** `respond.FromError` maps no SQLSTATE, so a 40P01 answered `500` `internal_error`. The idempotency middleware stores every status except `409`, `422` and `503` for 24 hours ([Idempotency](../api/idempotency.md)). A request sent with a key that lost a deadlock replayed its `500` to every retry with that key.
- **The documented deadlocks that reach a request:**
  - **D1: cancel or cross-interval plan swap against a paid flip**, while dunning's pause on the subscription is being written. A paid flip locks the invoice, then waits for the subscription to lift the pause (the comment on `subscription.PostgresStore.LockDunningPauseTx`). The cancel or swap holds the subscription, then waits for the invoice to void it. The paid flips are record payment, collect's credit-covered settle, credit-note issue and the issue step of create with `auto_issue`.
  - **D2: test-clock teardown against a period closer** ([ADR-066](066-threshold-money-semantics.md)). The teardown deletes the clock's invoices, then its subscriptions. A cancel, a swap or a period close holds the subscription, then reads the threshold invoice.
  - **D3: held usage release or discard against LiteLLM ingest.** They lock the same held rows in different orders.
  - **D4: cancel against a credit-note create, now removed.** The cancel took the invoice-number lock, then the credit-note invoice lock. Credit-note create (with or without `auto_issue`), the refund route, commit-relief create and recording a refund made in Stripe took them the other way. P1-08 ([#1649](https://github.com/gethardcount/hardcount/pull/1649)) removed it: the cancel writes its relief before it numbers its final invoice. It also has a credit-note create lock its invoice row before it numbers the note, so a relief that voids an unused invoice no longer deadlocks with a credit note on that invoice.
- **The same race already answers 409 when it does not deadlock.** When a payment lands between a cancel's funding read and its void, the void answers `409` `invoice_changed`, and [Errors](../api/errors.md) tells the client to read again and retry. Clients of cancel and plan change already handle a `409` followed by a resend.

## Decision

1. **`respond.DeadlockAsConflict(r, err)`** (`internal/api/respond/deadlock.go`). For a 40P01 it logs the raw error and the Postgres detail (which process waited for which) at WARN, as "request lost a database deadlock; answering 409", with the route. It returns `errs.InvalidState` with code `concurrent_change` and a message that names no cause, no SQL and no key, because the dashboard shows it. Every other error, nil included, comes back unchanged. A serialization failure (40001) is not converted: every application transaction runs at READ COMMITTED.
2. **Six handlers call it.** Each route is safe to send again because of what it saves before the transaction that can lose:

   | Route | Saved before the losing transaction | Why sending it again is safe |
   |---|---|---|
   | `POST /v1/subscriptions/{id}/cancel` | The invoice of a period that had already ended, which may already be charged | The period's compare-and-set does not bill it twice |
   | `PATCH /v1/subscriptions/{id}/items/{itemID}` | The same, through `CloseEndedPeriodFirst` | The same. Neither path returns an error once its transaction has committed |
   | `POST /v1/invoices/{id}/record-payment` | Nothing | — |
   | `POST /v1/invoices/{id}/collect`, credit-covered branch only | The credit applied to the invoice | Collect refuses an invoice with nothing due before it applies credit, so no credit is drawn twice |
   | `POST /v1/credit-notes/{id}/issue` | Nothing | Once its transaction commits, the draft→issued compare-and-set refuses a second issue |
   | `POST /v1/credit-notes` with `auto_issue`, issue step | The draft note, which the handler voids | Only when the void succeeded. When it failed, the draft stays, and the answer stays the stored `500` |

3. **Who runs each documented loser again:**

   | Loser | Who runs it again |
   |---|---|
   | The six routes above, as D1 or D2 losers | The client, after the `409` |
   | `DELETE /v1/test-clocks/{id}` (D2) | The client: a `DELETE` is not stored |
   | A period close inside a request (D2) | The request answers `409` `unbilled_period`; the billing run closes the period on its next tick |
   | `POST /v1/usage-events/held/release` and `/held/discard` (D3) | The client: usage routes skip the idempotency layer, so their `500` is not replayed |
   | LiteLLM ingest (D3) | The store, once (`IngestCallResolvingHeld`) |
   | Dunning's pause and cancel (`SetDunningPause`, `CancelForDunning`) | Dunning, on its next tick |
   | Stripe webhooks | Stripe, which sends the event again after a `5xx` |
   | The reconciler, the collector, the period close and the scheduled cancel | Their job, on its next tick |
   | Collect's card branch | Not converted. Stripe may have taken the money, so the stored `500` is the answer the idempotency layer chose, and the webhook settles the invoice |
   | The D4 writers: credit-note create, the refund route, commit-relief create and a refund made in Stripe | Not converted. Their only documented deadlock was D4, which P1-08 removed |
   | Any other request | Not converted. A 40P01 there is still a stored `500` |

## Why this design

- **It is the smallest change that frees the key.** Lock order, network calls and transaction bodies stay as they are.
- **A `409` is what the same race already answers** when it does not deadlock (`invoice_changed`), so clients need nothing new.
- **The conversion lives in handlers, not in services or stores.** Several callers there read `errs.ErrInvalidState` as a harmless outcome. A card settlement reads it as a voided invoice and acknowledges the webhook. Starting dunning reads it as "dunning disabled" and starts no run. The billing engine and the subscription service skip an activation. A deadlock turned into `ErrInvalidState` below the handler would be dropped instead of run again.
- **These deadlocks are rare.** Each needs two writers on the same records at the same moment, and D1 needs dunning's pause in flight as well.

## Alternatives considered

- **A. Run the whole transaction again at its owner.** The PostgreSQL manual says retrying a 40P01 "may also be advisable" ([Serialization Failure Handling](https://www.postgresql.org/docs/current/mvcc-serialization-failure-handling.html)). Rejected for now: about 60 production lines and 450 test lines, and "fn may run twice" becomes a rule on seven owners and on `DB.WithTenantTx`. A retried body also has effects outside its transaction:
  - `tax.PostgresStore.Record` commits each tax calculation on its own transaction, so each lost attempt leaves a `tax_calculations` row with no invoice;
  - a cancel's Stripe Tax idempotency scope includes `canceled_at` in nanoseconds, read again on each attempt, so each retry pays for a new calculation, and the swap and proration calls have no scope at all;
  - `audit.LogInTx` marks the request covered before its commit, which relies on "a change that rolls back answers non-2xx". A retry would mark the request on its first attempt and write no row on the second, then answer `2xx`.

  It would save one client resend in a race that is already rare. A retry added later builds on this ADR: the handler conversion stays as its last step.
- **B. Map 40P01 in `FromError` for every route.** Rejected. A `409` frees the key, which is safe only where everything the request saved earlier can run twice. Collect's card branch can deadlock in its settle after Stripe captured the charge. The idempotency layer stores that `500` on purpose.
- **C. Run the whole request again in the idempotency middleware.** Rejected. Charge now would claim the invoice again after a capture, `auto_issue` would make a second note, and the middleware cannot see the error once the handler has written the answer.
- **D. Fix the lock order of each pair** ([money-path playbook](../dev/money-path-robustness-playbook.md#5-test-lock-doctrine), item 6; [ADR-078](078-prepaid-commits-phase-1.md) decision 7). Not needed now:
  - D1 has no small fix. `LockDunningPauseTx` locks the subscription only after a pause has committed, on purpose. Locking it on every payment would make each payment wait behind closers that hold the subscription across their tax call.
  - D2: the teardown could lock the clock's subscriptions right after the clock row. After this ADR, neither side leaves a stored `500`.
  - D3: release and discard could lock in key order. Neither side is stored.
  - D4: P1-08 fixed it this way.

## Consequences

### Positive

- After a deadlock on the six routes, the same key works again.
- Each loss leaves a WARN line that names the losing statement, through the error's context, and who waited for whom, through the Postgres detail.

### Risks / open items

- **Revisit trigger.** The WARN line "request lost a database deadlock" names a pair. A pair that recurs gets a lock-order fix and its pair test, under the money-path playbook (section 5, item 6).
- **A resend repeats the work the lost attempt did before Postgres stopped it.** For a cancel or a prorated item change that includes the tax calculation, whose row stays with no invoice. The `invoice_changed` `409` already has the same cost.
- **Collect does not finish after its `409`.** The credit stays applied, so the invoice is open with nothing due. Collect sent again answers `422` "invoice has no amount due": its pre-check, and the charge claim behind it, admit only an invoice with an amount due. The same state follows any failure of that settle today, not only a deadlock. A dunning retry settles such an invoice (it marks an invoice with nothing due paid), and so does the auto-charge sweep while the invoice still waits for its automatic charge. Collect itself does not.
- **Other transactions.** A 40P01 in any other request, the D4 writers included, stays a stored `500`. None has a documented deadlock now.

## References

- `internal/api/respond/deadlock.go`, and the six call sites in `internal/subscription/handler.go`, `internal/invoice/handler.go` and `internal/creditnote/handler.go`.
- `internal/api/deadlock_conflict_e2e_test.go`: a real deadlock between a cancel and a payment.
- [ADR-066](066-threshold-money-semantics.md): the D2 deadlock.
- [Idempotency](../api/idempotency.md) and [Errors](../api/errors.md#409-conflict).
