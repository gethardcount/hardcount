# ADR-118: Cancel and plan-swap relief is written in the change's transaction

**Date:** 2026-10-12
**Status:** Accepted (amended 2026-10-19: after a cancel commits, the relief
is issued before the final invoice is collected, so the relief reduces the
final invoice; see the
[amendment](#amendment-2026-10-19-the-relief-is-issued-before-the-final-invoice-is-collected))

## Summary

A mid-period cancel or immediate cross-interval plan swap owes the customer
relief for the unused, prepaid part of the period. Velox now writes all of it
in the same transaction as the cancel or swap. There are three kinds:
- a credit note that credits the customer's balance, for a paid invoice;
- a credit note that reduces the amount due, for an unpaid invoice that was
  partly used;
- a void, for an unpaid invoice whose period was not used at all.

If any part fails, the cancel or swap rolls back and is retried. The
post-commit fallback paths that could lose relief silently are deleted.

After the commit, the relief's credit notes are issued before the cancel's
final invoice is collected, so the relief reduces that invoice (amended
2026-10-19).

## Context

ADR-057 made the relief atomic only when every invoice funding the period was
paid. If any funding invoice was unpaid, for example a mid-period upgrade
invoice not yet paid, the whole relief ran after the cancel or swap had
committed. That ran through `BillOnCancel` and `BillOnPlanSwapImmediate`.

A failure there was logged at ERROR and the change stood. That left:
- a canceled subscription whose unpaid, unused invoice was still due;
- a paid share never credited to the balance.

There was no retry and no reconciler. Re-calling Cancel on a canceled
subscription returns early. A crash between the commit and the post-commit
step lost the relief without even the log line.

The same gap existed on two plan-swap paths:
- the non-atomic cross-interval swap refunded first and swapped after;
- the atomic swap's post-commit fallback.

It was found by the fail-closed sweep of 2026-10-11, whose rule is: an error
fails, it is never replaced by a default.

## Decision

`createUnusedDraftsTx` (now `reliefTx` in `internal/billing/engine.go`) writes
one relief per funding share on the caller's transaction:

- **Paid share:** `CreateAdjustmentDraftTx` writes an issue_pending credit
  note. As before, issuing it credits the customer's balance.
- **Unpaid share, partly used, or with a charge in flight:**
  `CreateAdjustmentDraftTx` writes a credit note. Issuing it reduces
  amount_due. For an in-flight charge, Issue defers until the charge settles
  (ADR-059).
- **Unpaid share, fully unused, nothing paid, no charge in flight:**
  `invoice.Service.VoidUnchangedTx` voids the invoice in place. The applied
  customer credits are handed back and a funded commit grant is retired, both
  in the same transaction (unchanged void behavior).
  - The void refuses with `invoice_changed` if the invoice differs from the
    engine's snapshot. The compared fields are status, payment status,
    amount due, amount paid and total.
  - A refusal rolls the change back, and the retry decides again.

Any error rolls the cancel or swap back. That covers a funding-lookup failure,
an allocation failure, a draft or a void.

One case is not an error: an all-paid funding set with less room left than
the unused part. Amended 2026-10-20: the allocator places what fits and logs
the residual at ERROR, and the change proceeds. Before, it returned
`errHeadroomExhausted` and the change proceeded with no relief at all. A set
already fully credited still gets none (ADR-061, decision 2026-10-20).

The result is a `domain.Relief` (draft ids, voided invoices, and the balance
credit for the timeline). After commit, `FinishRelief` issues the drafts and
runs each void's tax reversal and `invoice.voided` webhook. Every one of those
legs is already recovered by its own sweep:
- `RetryPendingClawbackIssue` for drafts;
- `RetryPendingTaxReversal` for void tax.

So a crash before `FinishRelief` loses nothing.

Deleted:
- `Engine.BillOnCancel`;
- `Engine.BillOnPlanSwapImmediate`;
- `settleUnusedAcrossFunding`;
- `relieveUnpaidPrebill` (reduced to the pure `unpaidRelief` sizing helper);
- `IssueCancelDrafts` and `IssueSwapDrafts` (replaced by `FinishRelief`);
- the non-atomic `applyCrossIntervalPlanSwap`. `UpdateItem` now refuses an
  immediate cross-interval swap with `swap_requires_atomic_path`; the handler
  runs it through `UpdateItemTx`.

### Edge cases settled in review

- **Draft funding invoice.** With `pause_collection` set to keep_as_draft, a
  cycle invoice stays a draft. A partly used draft still gets its relief
  credit note, and Issue holds that note until the invoice is finalized, as it
  does for an in-flight charge. If the draft is voided instead, the note is
  voided too. Only this automated note may target a draft; an operator credit
  note still needs an issued invoice.
  - A draft whose tax is not yet calculated has no final total to size the
    relief against. The change is refused with `funding_invoice_tax_pending`,
    and the tax retry clears that state.
- **Fully unused is a time fact.** An unpaid invoice is voided when the change
  lands on its period start's day (zero used days) and nothing was paid. It is
  taken out of the allocation before the rest is shared out. The old amount
  test (`relief >= amount_due`) let the credits applied to that invoice spill
  onto a paid invoice as balance credit, while the void also handed them back,
  so the customer got them twice.
- **Scheduled cancels** treat only the fire's own race outcomes as benign
  (`scheduled_cancel_superseded`). A relief refusal is surfaced, so the scan
  retries it; before, it was skipped as a race at INFO level.
- **Credit-note Issue** picks its branch from a read taken before its
  transaction. The amount-due reduction now applies only to a finalized,
  unpaid invoice, and otherwise refuses with `invoice_not_open_receivable`.
  So a payment landing in between sends the note back to the reconciler,
  which re-applies it against the paid invoice as a balance credit.

## Why this design

The principle: a money effect that must happen because of a state change is
written in that change's transaction. A reconciler is used only for the
external legs that cannot be.

The relief has no external leg that must run before commit. The void's tax
reversal and the credit notes' Stripe legs already run after commit, with
sweeps behind them. So nothing stopped the whole relief from being in-tx
except history: ADR-057 did the dominant all-paid case first.

The snapshot check on the void exists because the engine decides "void"
from a read taken before the transaction. A payment that lands in between
must not be annulled. Refusing and retrying is simpler than deciding again
inside the lock, and the retry reads fresh data.

## Alternatives considered

- **A. A durable "relief owed" row plus a sweep.**
  - What: record the owed relief in the change's transaction and let a new
    sweep issue it.
  - Why rejected: it adds a table and a reconciler, when the credit-note
    drafts and the void already are durable records with sweeps behind them.
- **B. Always a full-amount credit note instead of a void.**
  - What: one shape for every unpaid share, so no in-transaction void is
    needed.
  - Why rejected: a credit note cannot hand back customer credits applied to
    the invoice, or retire a funded commit grant. The invoice would also end
    finalized at $0 instead of voided. *(Amended 2026-10-12, ADR-061: a
    credit note now returns applied credits it displaces at issue. The other
    two reasons stand.)*
- **C. Keep the post-commit path but return its error.**
  - Why rejected: the cancel already happened, so the error misreports it,
    and a retry hits "already canceled" without restoring the relief.

## Consequences

### Positive
- No cancel or swap can commit without its relief. The silent-loss sites are
  gone (the sweep's sites a–d).
- One relief path instead of two, about 400 fewer lines of production code.

### Risks / open items
- A cancel or immediate swap now fails on a transient error it used to
  survive: a funding lookup, a `CreditedCents` read, a credit-note cap race,
  or a void refusal.
  - Dunning and the scheduled-cancel scan retry on their own.
  - An operator or API caller sees a 5xx (409 for `invoice_changed`) and
    retries.
- Dunning's terminal cancel can now void the invoice in the cancel's
  transaction. Its following write-off then fails once on the voided
  invoice, and the run resolves on the next attempt. This already happened
  on the old post-commit void; a follow-up can re-read the invoice after the
  cancel.
- Site e (the clawback credit note on the handler's no-transaction path) is
  unreachable in production and is retired in a follow-up PR, along with
  `CreateAndIssueAdjustment`.
- `reverseInvoiceTax` subtracts `CreditedCents`, which counts draft credit
  notes. A void that lands while a relief draft on the same invoice is still
  pending can under-reverse. This is pre-existing and fixed separately.

## References

- ADR-031 (cancel proration credit)
- ADR-050 (unpaid prebill relief)
- ADR-056 (atomic coordinator transactions)
- ADR-057 (atomic cancel credit, all-paid case)
- ADR-059 (in-flight charge guard)
- ADR-061 (credit-note Issue atomicity)
- Code: `internal/billing/engine.go` (`reliefTx`, `unpaidRelief`,
  `FinishRelief`), `internal/invoice/service.go` (`VoidUnchangedTx`,
  `AfterVoid`), `internal/domain/relief.go`

## Amendment 2026-10-19: the relief is issued before the final invoice is collected

**Decision.** The relief reduces the final invoice of a cancel. This is the
rule of [ADR-088](088-credit-balance-applies-to-all-invoices.md): the credit
balance applies to every invoice when it is finalized, the final invoice of
a cancel included. The owner, Sagar, decided it on #1272 on 2026-10-18. A
card on file and no card must end with the same amounts.

**What was wrong.** After the cancel committed, `Cancel` and
`FireDueScheduledCancel` ran `FinalizeOnCreateInvoice` first and
`FinishRelief` second. Collecting the final invoice applies the customer's
balance and charges the card in one call (`processAutoCharge`). The relief's
credit notes were not issued yet, so a paid share's credit was not on the
balance yet:

- With a card on file, the card was charged the whole final invoice. The
  relief then landed on the balance of a customer with no subscription.
  Getting it back took a manual refund.
- With no card, the email asking for a card, and the no-payment dunning
  run, were for the whole amount. The auto-charge sweep applied the relief
  on a later tick.

The cross-interval swap (`FinalizeCrossIntervalSwap`) already ran the relief
first. The cancel order came from a code comment of 2026-05-18 ("so the
credit grant doesn't pre-apply against this invoice"). That comment is older
than ADR-088, and no later ADR decided the order again.

**What changed** (#1286). All three callers run their post-commit legs
through one helper, `Service.finishReliefThenCollect` in
`internal/subscription/service.go`. It runs `FinishRelief`, then
`FinalizeOnCreateInvoice`. The writes inside the transaction, and their
order, are unchanged. Neither of those writes applies credits.

The helper runs both legs on a context the caller cannot cancel, as
collection already did. The caller's context can end right after the commit:
an API request's timeout or a client disconnect, a billing run stopping on
shutdown or a lost lease, a dunning tick stopping. Before, the relief's
`Issue` then failed on its first database call while collection still ran,
so the card was charged the whole final invoice.

What the relief now does to the final invoice, by share:

- **Paid share.** Its credit note credits the balance, and collection
  applies that credit to the final invoice. The card is charged only the
  rest. A relief larger than the final invoice pays it with no charge, and
  the rest stays on the balance.
- **Unpaid share, partly used.** Its credit note reduces the amount due of
  its own invoice, as before. Customer credits that the note takes off that
  invoice go back to the balance, and now apply to the final invoice too.
- **Void.** Unchanged. The void returns the credits applied to the voided
  invoice inside the cancel's transaction, before either leg runs.

**Residual, not closed.** The relief reduces the final invoice only when
`FinishRelief` issues the credit notes before the final invoice is
collected. In four cases the final invoice is collected first, without the
relief, as it was before this amendment:

- The process crashes between the commit and `FinishRelief`.
  `RetryPendingClawbackIssue` and the auto-charge sweep both recover their
  half, on separate schedules and in no fixed order.
- `Issue` fails during `FinishRelief`. `RetryPendingClawbackIssue` issues
  the note later.
- An auto-charge sweep tick claims the final invoice between the commit and
  the end of `FinishRelief`.
- The credit note's invoice is unpaid and still a draft, or has a charge in
  flight. `Issue` holds the note and returns no error (ADR-059 for a charge
  in flight, this ADR for a draft), so nothing is logged and collection
  runs. `RetryPendingClawbackIssue` issues the note once that invoice is
  finalized or its charge settles. If the charge succeeded, the invoice is
  paid by then, so the note credits the balance.

In each case no money is lost. The customer pays the final invoice in full
and gets the relief later: as balance credit, or as a lower amount due on
the invoice the note was held for. Closing the first three would move the
credit grant into the cancel's transaction, which ADR-118 keeps after
commit. That waits until one of them is seen in production. The fourth is
the hold doing its job: what the note can relieve is not known until the
charge settles or the draft is finalized.

**Tests.**

- `internal/billing/cancel_relief_reduces_final_invoice_integration_test.go`
  runs on real Postgres. It covers the immediate and the scheduled cancel,
  each with a card on file and with no card. In one case the relief is
  smaller than the final invoice; in the other it is larger.
  A third case has an unpaid day-1 invoice with customer credits applied:
  the credits the relief returns reduce the final invoice.
- `internal/subscription/relief_before_collect_test.go` checks the order on
  all three callers, and that both legs still run on a live context when
  the caller's context ends right after the commit.

## Amendment 2026-10-22: a relief void runs every void's post-commit legs

**What was wrong.** A void has four legs after its transaction commits:

1. Stop collection at Stripe: expire the invoice's live Checkout sessions,
   then cancel its PaymentIntent.
2. Reverse the tax.
3. Send `invoice.voided`.
4. Resolve the invoice's dunning run as `invoice_voided`.

`AfterVoid` ran only
the tax reversal and the webhook. The other two legs ran in the two HTTP
handlers (the invoice void and the dunning resolve), so the relief's void
(`VoidUnchangedTx`, then `FinishRelief`) skipped them.

The trigger: a flat in_advance renewal, its day-1 charge fails and dunning
starts, and the operator cancels or swaps the plan that day. The cancel
voids the prebill. Its dunning run stayed active until the run's next due
tick, up to the grace period (days) later. A hosted-page Checkout session
stayed payable for up to an hour, with the paid-on-voided anomaly (ADR-068)
as the only catch.

**What changed.** `invoice.Service.AfterVoid` runs all four legs, in that
order, on a context the caller cannot cancel. Every void reaches it: `Void`
calls it, and so does `FinishRelief` after a relief void. The two handlers
no longer run legs of their own. The dunning handler voids, and its
`ResolveRun` then loses the resolve CAS and returns the run the void
resolved, so the run gets one resolved row and one `dunning.resolved`.

**Consequence for dunning's final cancel.** When the final action cancels
the subscription and the cancel voids the run's own invoice, the run now
ends resolved as `invoice_voided` under either final invoice action, with no
escalation and no escalation email. With `mark_uncollectible` it already
ended that way (the re-read before the write-off). With `none` it used to
escalate over the voided invoice. The case needs the last retry to fall on
the period's first day.

**The risk bullet above** ("Its following write-off then fails once on the
voided invoice") no longer holds. The void resolves the run. The re-read
before the write-off stays as the fallback if that resolve fails.

**Tests.** `internal/billing/dunning_cancel_voids_prebill_integration_test.go`
(cancel, same-day cross-interval swap, dunning's final cancel under both
invoice actions), `internal/invoice/void_effects_integration_test.go` and
`internal/dunning/resolve_void_integration_test.go`, on real Postgres.
