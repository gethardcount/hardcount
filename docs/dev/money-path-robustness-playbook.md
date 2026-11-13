# Money-path robustness playbook

The checklist for a change to money or a billing state, so it never charges twice, loses an effect or leaves a record stuck.

## When this applies

Your change writes, reacts to, or branches on the state of an invoice, a payment, a credit, a [dunning run](../glossary.md#payments-and-collection), a subscription or a tax record.

1. [List every place](#list-every-place-first) that touches the state, before you write code.
2. Answer [the checklist](#the-checklist): yes, no, or "n/a, because …". A "no" blocks the pull request.
3. Write [the tests](#the-tests-a-money-change-needs).

The reviewer answers the checklist again from the code and reruns your searches.

## List every place first

Check the state, such as an invoice's `paid` status, not your diff. A money bug is usually in another caller, a race, or a crash between two writes.

The **site-set** is every place in the code that touches one state:

- Every writer: handler, service, billing engine, scheduler, webhook, operator action, repair job.
- Everything that fires an effect on a change: an email, a webhook, a Stripe call, a ledger write.
- Everything that branches on the state.
- What calls these, and what they call.
- The line after each commit and each external call: a crash can stop there.

Put the list in the pull request, ticked against search output:

```bash
grep -rnE "MarkPaid|resolveRunAt|Enqueue|\.Send" internal/<domain>/
```

## The checklist

Each lettered class is one way a billing change goes wrong, with an example. Most numbered items name code to copy: do not invent a second way. Cite them in code comments as `playbook class J` or `playbook item 8`, and never renumber them.

### A. Done twice

A charge key built from `updated_at` moved on an unrelated write and started a second payment.

1. **Does every money write have a stored key that makes a repeat collide?** An API request has the `Idempotency-Key` header. Engine, scheduler and webhook work has none, so it uses a unique index on its cause (`idx_invoices_proration_dedup`) or item 5's `UPDATE`.
2. **Is each Stripe [idempotency key](../glossary.md#pricing-and-usage) built from stored ids, the attempt and the purpose?** Never a random value or a stand-in such as `updated_at`, which every writer of the row changes. Use a counter that only the attempt moves, like `invoices.charge_attempt_seq` ([ADR-105](../adr/105-charge-idempotency-key-seed.md), [ADR-137](../adr/137-charge-stamps-invoice-before-confirming.md)). Copy: `internal/payment/stripe.go:ChargeIdempotencyKey`.

### B. Half saved

A cancel committed, and a crash lost its final bill.

3. **Is each effect saved in the same transaction as its change?** A database write uses the caller's `*sql.Tx` ([ADR-056](../adr/056-atomic-cross-interval-plan-swap.md)). A webhook event or a money email is queued on it with `OutboxStore.Enqueue` ([ADR-040](../adr/040-outbox-always-on.md)). An effect that cannot join leaves a marker for a later pass, such as `no_pm_notified_at`. Check: `internal/arch/webhook_emit_catalog_test.go`, `internal/arch/email_outbox_standalone_test.go`. Review: follow each `Enqueue`'s transaction back to its `BeginTx`. No test catches a transaction opened only to call `Enqueue`.

### C. Wrong source of truth

A webhook was marked handled, its effect failed, and Stripe's retry was dropped.

4. **Does only Stripe's word change what was paid?** Money state comes from a signed webhook or Stripe's answer to a server call, never a browser redirect. A webhook handler is safe to run twice and in any order. It runs the effect, then marks the event handled (`IngestEvent`). It returns every error but `ErrNotFound`, which no retry can fix, so Stripe sends the event again. Copy: `internal/payment/stripe.go:HandleWebhook`.

### D. Race

Dunning cancelled a subscription while its invoice was being paid.

5. **Is each state change one conditional `UPDATE`, with effects fired only by the winner?** The `UPDATE`, a CAS (compare-and-set) in the code, names the states the row may come from ([code standard](code-standard.md#errors-state-and-structure)). A call that changed no row lost the race, and fires nothing. Copy: `internal/dunning/postgres.go:UpdateRunIfActive`.
6. **Do all writers go through one function that holds the guard and the validation?** That includes a `…Tx` variant, a bulk save and a recipe. One that skips a check names the earlier check that covers it. If no function fits, extend one first. Copy: `internal/dunning/service.go:resolveRunAt`, `internal/dunning/service.go:normalizeAndValidatePolicy`, `internal/invoice/postgres.go:markPaidLockedTx`.
7. **Does each action that cannot be undone re-read its precondition right before it fires?** That means cancel, [void](../glossary.md#subscriptions-and-invoices), pause and mark-uncollectible: a check made before a Stripe call is stale after it. If the read fails, nothing fires, and the record is tried again. Copy: the late paid re-check in `internal/dunning/service.go:exhaustRun`.
8. **Does a transaction lock the invoice row before it writes a row that belongs to the invoice?** A settle (the transaction that records a payment) locks the invoice first. A writer that writes its row first deadlocks with it: its foreign-key check waits for the invoice, and the settle for the row. Copy: the "Lock order" comments in `internal/dunning/postgres.go`.

### E. Crash or hidden error

A [test-clock](../glossary.md#testing-with-simulated-time) advance panicked and stayed `advancing`.

9. **After a crash on any line, does something that exists finish the work?** Name the queued row, marker column or repair job that picks the work up, and confirm it reads this state. A comment that promises a retry names the code that does it. Background work that panics marks its record failed, where an operator finds it. Copy: the test-clock advance (`internal/testclock/service.go`).
10. **Is every error on a money write returned, or logged as an error?** No `_ =` on a store write or a queued send. Older code does not all follow this yet. A failure that leaves money uncollected if it lasts is logged at `ERROR`, not `WARN`.

### F. Part of a set

A credit read one invoice for a period that had two.

11. **Does an effect that covers a set read the whole set?** No `LIMIT 1`, no single-row lookup, and no status filter that drops a paid, void or uncollectible sibling. Several invoices can bill one period, its funding invoices: read them with `FindFundingInvoicesForPeriod`, which lint enforces in two packages ([code standard](code-standard.md#go-rules-a-tool-checks)). A question about every row ("does any subscription bill in another currency?") is one query with no page limit.

### G. Never ends

An invoice with no card was retried for auto-charge forever.

12. **Can every record reach an end state?** Every state has a writer that moves it on, also when the final action fails. A dunning retry with no definite answer returns `dunning.ErrTransientSkip` and uses up no attempt. A job that works a list reads it to the end with a cursor on the columns it orders by, or stamps each row it looked at. Then rows it could not finish never hold back the rows behind them.

### H. Tenant leak

[Row-level security](glossary.md#database-access) was enabled but not forced, so the table's owner read every tenant's rows.

13. **Is every tenant's data still fenced?** A new table with `tenant_id` gets row-level security in the same migration: `ENABLE`, `FORCE` and a `tenant_isolation` policy (`internal/platform/postgres/rls_enumeration_test.go`). A query that skips it (`TxBypass` or `db.Pool`) filters by tenant as [Architecture](../architecture.md#design-rules) says, and by [`livemode`](../glossary.md#accounts-and-access) where the table has it. The tenant comes from the signed-in key or session, never from the client.

### I. Wrong time or amount

A staleness check compared test-clock time with the database's `now()`.

14. **Does every time come from the right clock?** A month or year step goes through `domain.AddBillingInterval` with the tenant's time zone, never the host's ([ADR-058](../adr/058-billing-date-math-tenant-timezone.md)). A compare has the same clock on both sides: never test-clock time against the database's `now()` or a Stripe timestamp.
15. **Is the money math exact?** Whole cents, no floats ([code standard](code-standard.md)), and currency codes in upper case.

### J. Stamped late

One clock advance crossed two retry dates, and both retries carried its end time.

16. **Does work that runs late stamp the time it was due?** That is any field for when a billing event happened: `resolved_at`, `paid_at`, a timeline row. Pass the due time in, as `resolveRunAt`'s `at` parameter does. A bookkeeping field (`updated_at`, `issued_at`, audit time) carries the current time.

## The tests a money change needs

A race or a crash between two writes always gets an automated test. A money rule gets one unless an existing test covers the same pattern. Write the tests whose items your change touches. [How we test](../testing.md) has the layers and the hand checks.

1. **Collision (items 1, 4).** Run the change twice, together and in turn. Assert one effect, the error code (`invoice_proration_source_taken`) and an unchanged second run. Copy: `internal/invoice/postgres_proration_dedup_integration_test.go`, `internal/billing/engine_idempotency_integration_test.go`.
2. **Real Postgres (items 3, 13).** Make a transaction's second write fail and assert the first is gone. For tenant isolation, write as two tenants and read as one with no `WHERE`: it must see only its own rows. Copy: `internal/platform/postgres/rls_isolation_test.go`.
3. **A fake that races (items 5, 7).** A concurrent-resolver fake commits the competing change at the external call, or between the read and the write. Assert by count that the winner fired once and the loser changed nothing. Copy: `resolvingCanceler` and `resolvingRetrier` in `internal/dunning/resolve_on_settle_test.go`.
4. **Break it once.** Remove the `WHERE` condition, the rows-changed check, the re-read or the line the fix added, and watch a test fail. A line no test misses is not proven.
5. **One advance, many due times (item 16).** Advance a test clock across two or more due times, or two days past one. Assert each record carries its own. The final state alone proves nothing. Copy: `internal/dunning/service_test.go:TestProcessDueRunsForClock_RecoveryStampsContractedInstant`, `internal/api/activity_parity_dates_integration_test.go`.
6. **A race for each pair of writers (item 8).** Two writers that lock the same rows get one real-Postgres test that runs them together. Copy: `internal/payment/settle_once_concurrent_integration_test.go:TestSettleSucceeded_ConcurrentSources_RecordsOnce`, `internal/creditnote/number_lock_order_integration_test.go`.
7. **Compare with today (a change to an amount or a date).** Run the old and the new calculation over the shapes real writers produce. Every difference falls in a class you name, each with one case worked by hand and its expected value. No case may get worse than today: where the new rule cannot be right, keep today's answer and name the case.
8. **Walk a paged list (item 12).** Make the key order differ from insert order and from update order across a page boundary, and leave rows that stay listed. Assert every row comes back once and the walk ends.
