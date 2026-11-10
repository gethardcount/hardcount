# ADR-136: A deadlock loser answers 409 at its handler; no in-process retry

**Date:** 2026-11-09
**Status:** Accepted (amended 2026-11-10: a lock wait that Postgres ended answers the same `409`, and six more routes answer it; [amendment](#amendment-2026-11-10-a-lock-wait-postgres-ended-answers-the-same-409-on-six-more-routes); manual invoice create converts too, [amendment](#amendment-2026-11-10-later-manual-invoice-create-converts))

## Summary

Postgres breaks a deadlock by aborting one of the transactions in it, with SQLSTATE 40P01. Six handlers now answer that abort with `409` `concurrent_change` instead of `500`. The idempotency layer does not store a `409`, so the client sends the same request again with the same `Idempotency-Key`. Before, the `500` was stored, and the key replayed it for 24 hours. Hardcount does not run the transaction again itself, and no lock order changes. `respond.DeadlockAsConflict` does the conversion, only in a handler, and only where what the request saved before the transaction that lost is safe to run twice.

**Current rule (amended 2026-11-10).** The helper is now `respond.ConcurrentChangeAsConflict`. It also converts a statement whose wait for a lock Postgres ended at `lock_timeout` (SQLSTATE 55P03). Twelve routes call it: the six above, plus subscription create and activate, item add and item remove, customer update and billing profile. Those six and the item change above are the request-path takers of the per-customer lock that every writer of a customer's rules waits on. Every other route still answers a 40P01 or a 55P03 with a stored `500`.

## Context

- **The bug.** `respond.FromError` maps no SQLSTATE, so a 40P01 answered `500` `internal_error`. The idempotency middleware stores every status except `409`, `422` and `503` for 24 hours ([Idempotency](../api/idempotency.md)). A request sent with a key that lost a deadlock replayed its `500` to every retry with that key.
- **The documented deadlocks that reach a request:**
  - **D1: cancel or cross-interval plan swap against a paid flip**, while dunning's pause on the subscription is being written. A paid flip locks the invoice, then waits for the subscription to lift the pause (the comment on `subscription.PostgresStore.LockDunningPauseTx`). The cancel or swap holds the subscription, then waits for the invoice to void it. The request-path paid flips that answer `409` are record payment, collect's settles with no card (credit-covered, or nothing left due), credit-note issue and the issue step of create with `auto_issue`. The card settle reaches D1 too: the Stripe webhooks and collect's card branch. Neither answers `409` (table 3).
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
   | `POST /v1/invoices/{id}/collect`, its settles with no card only | In the credit-covered branch, the credit applied to the invoice | Sent again, Collect finds nothing due, so it applies no credit and marks the invoice paid (decision 4) |
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

4. **Collect settles a finalized, unpaid invoice with nothing due.** A credit-covered settle that fails after its credit was applied leaves one, a `409` included. Before, Collect answered `422` "invoice has no amount due" there, so its resend could not finish. Now `collectNothingDue` marks the invoice paid with no card and no charge claim, through `Service.SettleZeroDueWithoutClaim`:
   - It calls the store's `SettleZeroDue`, the dunning retry's settle. It re-reads the invoice under its row lock and settles only a finalized one with nothing due, tax ok and payment pending or failed. It refuses while a charge hold is set, because that charge may have taken the money. Collect then answers `409` `payment_in_flight`.
   - It does not call `Service.SettleZeroDue`. That one calls `MarkPaid`, which reads neither the amount due nor the hold under its lock, and is safe only inside the caller's own claim.
   - The dunning resolve and the collect audit row fire only when this call settled the invoice. A writer that settled it first owns them, and Collect answers `422` "invoice is already paid".

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
- **E. Answer `503` through `errs.Unavailable`.** That is the codebase's shape for "the request failed, a second run is safe, do not store the answer". It answers `503` with `Retry-After`, keeps the cause on the error, and logs once in `FromError`. [Idempotency](../api/idempotency.md) defines a `503` this way. Rejected: the same race already answers `409` `invoice_changed` when it does not deadlock, so clients of cancel and plan change already read a `409` and send again. A `503` would give one race two answers, and it says the server is unavailable when it is not. The cost is a second shape for the same meaning. The helper logs its own WARN line and drops the cause from the error it returns, because `FromError` renders an `ErrInvalidState` message as it is. Idempotency names `409` `concurrent_change` beside the `503`, so a client finds both.

## Consequences

### Positive

- After a deadlock on the six routes, the same key works again.
- Each loss leaves a WARN line that names the losing statement, through the error's context, and who waited for whom, through the Postgres detail.

### Risks / open items

- **Revisit trigger.** The WARN line "request lost a database deadlock" names a pair. A pair that recurs gets a lock-order fix and its pair test, under the money-path playbook (section 5, item 6).
- **A resend repeats the work the lost attempt did before Postgres stopped it.** For a cancel or a prorated item change that includes the tax calculation, whose row stays with no invoice. The `invoice_changed` `409` already has the same cost.
- **The dashboard does not offer Collect on an invoice with nothing due.** After collect's `409`, the invoice page reloads and shows the invoice open with $0 due and no Collect button, so an operator there cannot send it again. The API resend settles it (decision 4). So does a dunning retry, and the auto-charge sweep while the invoice still waits for its automatic charge.
- **Other transactions.** A 40P01 in any other request, the D4 writers included, stays a stored `500`. None has a documented deadlock now.

## Amendment 2026-11-10: a lock wait Postgres ended answers the same 409, on six more routes

**What changed.** When a statement waits for a lock longer than `lock_timeout`, Postgres ends it with SQLSTATE 55P03, and outside a savepoint that aborts the transaction. Hardcount sets no `lock_timeout` itself. Postgres raises 55P03 today only when the operator set one on the app role (`ALTER ROLE hardcount_app SET lock_timeout = …`). Before this amendment, that answered a stored `500` on every route.

1. **The helper is renamed `respond.ConcurrentChangeAsConflict`** and also converts 55P03 (`postgres.IsLockTimeout`). It writes its own WARN line, "request's lock wait ended at the lock limit; answering 409", with the raw error and the route. The code and the message are the same as for a deadlock. A statement timeout (57014) is not converted: it measures the statement's own time, not a wait for another change.
2. **The six routes above convert 55P03 unchanged.** Their condition is about what the request committed before the transaction that failed, not about why Postgres stopped that transaction.
3. **The rule for a new route.** A handler converts a lock timeout when both hold:
   - its write takes a lock that queues by design. That is the customer-rules lock (`postgres.LockCustomerRulesTx`). Every writer of one customer's rules waits in line behind each tax call made inside a transaction. So parallel creates for one customer wait the sum of those calls, with Stripe healthy;
   - the condition of decision 2: everything the request committed before the transaction that failed is safe to run twice.
4. **Six routes are added.** With the item change above, they are the request-path takers of the customer-rules lock. They convert a deadlock too. Each returns an error only from inside its one transaction, and its steps after the commit only log:

   | Route | The one transaction | Before it | After the commit |
   |---|---|---|---|
   | `POST /v1/subscriptions` | `subscription.Service.Create` → `PostgresStore.CreateWithBill` | Reads and checks only | `FinalizeOnCreateInvoice` returns nothing |
   | `POST /v1/subscriptions/{id}/activate` | `Service.Activate` → `ActivateDraftWithBill` | Reads and checks only | The same |
   | `POST /v1/subscriptions/{id}/items` | `Handler.atomicAddItemWithProration`, or `Service.AddItem` when nothing prorates | `CloseEndedPeriodFirst`, whose period compare-and-set never bills a period twice | `commitProrationTax` and `issueClawbackDrafts` return nothing |
   | `DELETE /v1/subscriptions/{id}/items/{itemID}` | `Handler.atomicRemoveItemWithProration`, or `Service.RemoveItem` when nothing is credited; both run `Service.RemoveItemTx` | `CloseEndedPeriodFirst`, the same | `commitProrationTax` and `issueClawbackDrafts` return nothing, and the audit row's error is dropped |
   | `PATCH /v1/customers/{id}` | `customer.Service.Update` → `WithCustomerRulesLock` | Input checks only | The Stripe sync only logs |
   | `PUT /v1/customers/{id}/billing-profile` | `Service.UpsertBillingProfile` → `WithCustomerRulesLock` | Reads and checks only | The Stripe sync and the tax retry only log |

   The idempotency layer keeps no key for a `DELETE`, so the removal's `500` was never replayed. Its `409` tells the client that the removal can be sent again, as the item change's does.

5. **Routes left out.** They keep the stored `500` for a 55P03:
   - `POST /v1/subscriptions/{id}/schedule-cancel`, `PUT /v1/subscriptions/{id}/pause-collection`, `POST /v1/subscriptions/{id}/end-trial`, `POST /v1/subscriptions/{id}/extend-trial`, `POST /v1/subscriptions/{id}/retry-billing`, `PUT /v1/subscriptions/{id}/billing-thresholds`, and the plan and meter writes. They wait behind one writer of one subscription or plan, so their wait passes a lock limit only when one tax call or one usage read does.
   - The routes that number a document: credit-note create (`POST /v1/credit-notes`, before its `auto_issue` step) and the refund route (`POST /v1/invoices/{id}/refund`). (Manual invoice create, `POST /v1/invoices`, was here until 2026-11-10; see the amendment below.) They wait on the account's number row (`tenant_settings`). An immediate plan change across billing intervals holds that row across its usage read and its Stripe Tax call, because it numbers its refund credit note before it prices (`subscription.Service.applyCrossIntervalPlanSwapTx`). Until that swap prices first, these routes can wait behind it.
   - Every other route.

**Why the cause need not be known.** A 55P03 and a 40P01 both abort the transaction, so nothing it wrote is kept, and the answer depends only on what committed before it. The WARN lines tell the two apart for an operator.

**Consequences.**

- A resend repeats what the lost attempt did on other connections before Postgres stopped it. For create, activate and a prorated add, that includes the tax calculation, whose row stays with no invoice. Risks above says the same for a cancel.
- A route left out answers a 55P03 with a `500` that its key replays for 24 hours. The client reads the resource, then sends the request again with a new key ([Idempotency](../api/idempotency.md)).

**Tests.** `internal/api/respond/deadlock_test.go` (40P01 and 55P03 convert, each with its WARN line; 57014, 40001 and others pass through). `internal/api/lock_timeout_conflict_e2e_test.go`: a second session holds the customer-rules lock, or for the cancel the subscription row, and the server's pool runs as a role with `lock_timeout = '300ms'`. Each of the six routes, both branches of the add and of the remove, and the cancel answer `409` `concurrent_change` and change nothing. The same request, with the same key, then succeeds once.

## References

- `internal/api/respond/deadlock.go`, and the seven call sites in `internal/subscription/handler.go`, `internal/invoice/handler.go` (two in collect) and `internal/creditnote/handler.go`. Since the amendment of 2026-11-10, fourteen: five more in `internal/subscription/handler.go` (create, activate, item add, and both branches of item remove) and two in `internal/customer/handler.go` (update, billing profile).
- `internal/api/lock_timeout_conflict_e2e_test.go`: a lock wait that Postgres ended, on each of the six routes and on cancel.
- `internal/invoice/collect_open_at_zero_integration_test.go`: Collect on an invoice with nothing due, with and without a charge hold.
- `internal/api/deadlock_conflict_e2e_test.go`: a real deadlock between a cancel and a payment.
- [ADR-066](066-threshold-money-semantics.md): the D2 deadlock.
- [Idempotency](../api/idempotency.md) and [Errors](../api/errors.md#409-conflict).

## Amendment 2026-11-10 (later): manual invoice create converts

Manual invoice create (`POST /v1/invoices`) was left out because it took its
invoice number on its own connection before its insert, and the number
stayed taken. Since #1681 the number is taken inside the insert's one
transaction (`CreateNumberedAudited`), and everything before that
transaction only reads (the customer's test clock, the subscription check,
the tenant settings, the currency). So a lost lock wait or deadlock leaves
nothing behind, and the handler answers `409` `concurrent_change` through
`respond.ConcurrentChangeAsConflict`, for a bare header and for a create with
lines alike. A resend with the same key creates the invoice once.

Tests: `TestE2E_LockWaitEnded_AnswersConflict_SameKeyRunsAgain`, cases
`invoice_create` and `invoice_create_with_lines` (a second session holds the
account's invoice-number row).

## Amendment 2026-11-10 (ADR-139): the cancel's second plan is not a deadlock retry

The cancel now plans its bill before its transaction and plans it again once when the subscription moved in between ([ADR-139](139-cancel-prices-before-its-transaction.md)). That second plan follows a compare-and-set that wrote nothing, not a deadlock: a 40P01 or a 55P03 still answers `409` here, and is not run again in the process. Before a cancel's second plan, or its `409` `concurrent_change` after a second move, only an ended period's invoice and a `tax_calculations` row with no invoice can have been saved, as before a lost deadlock.
