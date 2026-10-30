# ADR-132: Returned credit goes back to the grants it came from

**Date:** 2026-10-30
**Status:** Accepted

## Summary

When a void, or a credit note on an unpaid invoice, hands back credit the
invoice drew from the customer's balance, that credit goes back to the
grants it was drawn from. It keeps their expiry and their kind (promotional,
commit or paid). A share whose grant is already past its expiry expires at
once, by the expiry sweep's rule. A share of a commit whose funding invoice
is voided is retired again. A credit note on a paid invoice follows one more
rule: it never gives the customer less than it says. Its drawn share goes
back to its grants only while they stay spendable, and the rest is new
credit. Before this, every return was a new grant with no expiry and no
kind, so expiring or promotional credit came back as never-expiring paid
credit. On a void this is what Stripe does. On a credit note Stripe restores
nothing; Hardcount restores, a deliberate difference stated below.

## Context

**The bug.** Three writers hand drawn credit back to the balance, and each
wrote a fresh grant row with no `expires_at` and no `grant_kind`:

1. **Void** (`credit.Service.ReverseForInvoiceTx`, run in the void's
   transaction): what the invoice still has applied.
2. **Credit note on an unpaid invoice** (`creditnote.issueUnpaidTx`): the
   applied credit the note relieves beyond what is still due (ADR-061
   amendment 2026-10-12).
3. **Credit note on a paid invoice** (`creditnote.Issue`, the credit
   channel): the whole credit amount, even the part the invoice had paid
   with credit.

A customer with credit due to expire next week, or with free promotional
credit, got it back as never-expiring paid-class credit. ADR-071 recorded
this as an accepted residual, ADR-061 left "what kind of credit comes back"
open with a trigger, and ADR-078 listed it as a commit limitation. All three
waited on per-block attribution. That data exists: since 2026-07-05 every
drain writes `metadata.drained_blocks = [{block_id, take_cents}]` on its
usage row.

**What the peers do** (verified 2026-10-30):

- **Stripe**, [Billing credits](https://docs.stripe.com/billing/subscriptions/usage-based/billing-credits),
  "Void invoices and issue credit notes": "Voiding an invoice with credits
  applied reinstates the applied balance to the credit grant. If the credit
  grant is past the expiry date, the reinstated credits expire
  immediately." And: "Issuing a Credit Note doesn't refund credit grants
  applied to an invoice. To restore those billing credits, you must create a
  new credit grant."
- **Lago**, [Void invoices](https://getlago.com/docs/guide/invoicing/void):
  "Only prepaid credits used to pay the invoice will be refunded to the
  customer's balance (as they have been prepaid)." The credit goes back to
  the prepaid wallet it came from.
- **Orb**, [Void invoice](https://docs.withorb.com/api-reference/invoice/void-invoice):
  "if the invoice used $10 of customer balance, that amount will be added
  back to the customer balance upon voiding." Its docs do not say how a
  void treats credit drawn from prepaid credit blocks.

## Decision

1. **One primitive.** `PostgresStore.ReturnCreditTx` writes every return, on
   the caller's transaction. Its callers are the void reversal and the
   credit-note grant (`GrantForCreditNoteTx`, both branches).
2. **Restore to the source blocks, last drawn first.** The return reads the
   invoice's usage rows newest first (by id, which sorts by creation time)
   and each row's `drained_blocks` in reverse. It skips what earlier returns
   on the invoice already restored (their `metadata.restored_blocks`). It
   lowers each source block's `consumed_cents` by its share.
3. **One row per source.** The whole amount is still one grant row, with the
   source id the dedup indexes key on (`source_invoice_reversal_id`, 0106;
   `source_credit_note_id`, 0093). The row is written with
   `consumed_cents` = the restored part and metadata `restored_blocks` and
   `restored_cents`. So it adds the whole amount to the balance, but only
   the unattributed rest to drainable capacity. `InvoiceCreditsTx` and the
   void's "still applied" sum are unchanged.
4. **Past expiry: the sweep's rule.** At the return's instant on the
   customer's clock (a test clock's frozen time for a pinned customer), a
   grant with `expires_at < now` is expired. Its restored share expires at
   once through the sweep's own write (`expireGrantRemainderTx`): consumed
   back up, an `Expired grant <id>` entry stamped at the return's instant.
   As in the sweep, a grant an owed draw still needs
   (`domain.CreditDrawOwedSQL`) is not expired. Its share stays live for
   that draw, and the sweep retires it later. The invoice the credit is
   returned for never counts as that draw (`CreditDrawOwedExceptSQL`): it
   is the draw being undone, even while it is still queued for collection. As in ADR-071 §2, retirement
   wins: a grant the sweep already wrote off is expired again even if a
   draw is now owed. A grant past expiry that the sweep has not written off
   yet is written off first, its remainder stamped at `expires_at`, exactly
   as the sweep would.
5. **Commit re-retire.** A share restored to a commit grant whose funding
   invoice is voided is retired again through `retireCommitSliceTx`
   (reason `commit_void_retire`). This comes before the expiry test. It reads
   the funding invoice's status, so it also covers a void that found the
   grant fully drawn and retired nothing.
6. **Credit note on a paid invoice: never less than it says.** The
   customer's balance rises by exactly the note's credit amount, the figure
   the note's PDF, hosted page and audit row state.
   - The note is split pro rata to how the invoice was paid. Its drawn share
     is `amount × attributed draws / invoice total`, in integer cents,
     cumulative over the invoice's earlier paid-branch notes:
     `floor((earlier + amount) × drawn / total) − floor(earlier × drawn / total)`.
     The rounding telescopes, so the notes never return more drawn credit
     than their share of it (`paidDrawnShare`).
   - The drawn share goes back to its blocks, last drawn first, only where
     it stays spendable: not past expiry, and not a commit whose funding
     invoice is voided (`spendableAt`). A share that would expire or be
     retired is new credit instead. Nothing on this branch expires or is
     retired at once.
   - The rest of the note is new credit with no expiry, as before.
   - Each such row carries metadata `paid_invoice: true` and
     `drawn_share_cents`, which the next note's cumulative share reads.
7. **Legacy rows.** A usage row from before 2026-07-05 has no
   `drained_blocks`. Its part goes back as new credit with no expiry, with a
   WARN naming the invoice. The amount handed back that way is
   (amount still applied) − (attributed and not yet restored). On a void or
   an unpaid note, every cent of the return is drawn credit. Such a return
   larger than the attributed outstanding plus the legacy rows is refused
   as corrupt, not minted.
8. **Locks.** The return's immediate caller holds the invoice row (the
   void, the credit note's issue, finalize's held relief). The return then
   takes the customer advisory lock, then the customer's ledger rows
   `FOR UPDATE`, then reads. Every restore is a CAS on the `consumed_cents`
   it read (`credit_block_changed` when it does not apply). The engine's
   cancel and plan-swap relief takes more first: `VoidUnchangedTx` and
   `LockInvoicesForDraftsTx` lock several invoice rows before any return.
   No path takes ledger rows and then an invoice row, so no cycle was found.
9. **Readers.**
   - Balance crossings (`credit.balance_*`, ADR-078) are emitted once, for
     the return's net change: a return whose share expires at once sends
     nothing when the balance ends where it started.
   - `ListGrantSummaries` reports a return row for its unattributed rest
     only, and leaves out a row with none. The restored part shows on the
     blocks it went back to: as remaining, or as expired or retired.
   - The balance totals (`GET /v1/credits/balance`, the credit list, the
     `credit.*` webhooks) take the restored part off both `total_granted`
     and `total_used`. They then match the grant list, and
     `total_granted = total_used + total_expired + total_retired +
     total_deducted + balance` still holds.
   - The ledger list reports a return row's `consumed_cents` without its
     restored part, so the field stays "how much has been used or expired".
   - The void audit row adds `expired_on_return_cents` and
     `commit_retired_on_return_cents` (inside `credits_reversed_cents`),
     when not zero.

## Why this design

**Credit keeps the terms it was given.** A return undoes a draw; it does
not create credit. Restoring the blocks puts the customer exactly where they
would be had the invoice never drawn, which is Stripe's void rule. A share
of an expired grant would have expired, so it does.

**Credit notes restore too, unlike Stripe.** Taking the credit away, as
Stripe does on a credit note, would be a new money rule against the customer.
Minting it all fresh is the bug. On an unpaid invoice the note relieves
applied credit, so restoring it gives back exactly the credit the customer
had, on its terms.

**A paid-invoice note never gives less than it says.** The note's documents
state an amount added to the balance. If that credit went back to a grant
that is expired, or to a commit whose funding invoice is voided, it would
vanish at once, and the customer would get less than the note says. A review
found both cases: a note on an invoice paid partly with promotional credit
that expired yesterday, and the engine's cancel relief on a paid prebill
(`reliefTx` → `CreateAdjustmentDraftTx`). The pro-rata split puts back only
what the invoice's own way of payment implies, and only where it can be
spent.

**One row, one primitive.** The dedup indexes and the void's sum read one
row per source; keeping that shape means none of them changes. The expiry
and commit writes reuse the existing single writers (ADR-071, ADR-080 §5), so
there is still one expiry write and one commit-retire core.

## Alternatives considered

- **A. Proportional restore over blocks on a partial return.** Spread a
  partial note over every block the invoice drew. Rejected: it needs a
  rounding rule per block, and last drawn first is what the drain's order
  implies (the last block drawn is the first the invoice would not have
  needed). The paid branch's pro-rata split is between drawn credit and
  cash, not between blocks.
- **A2. Drawn credit first on a paid invoice.** Place the note's credit on
  drawn credit before cash. Rejected after review: a note on an invoice paid
  with expired credit restored it to the expired grant, and the customer got
  less than the note said.
- **B. Hand back an expired share as live credit.** Rejected: it gives the
  customer credit they would not have had, and differs from Stripe's void.
- **C. Several rows per return, one per source block.** Rejected: the 0093
  and 0106 dedup indexes allow one row per source, and a second row per
  source breaks them.
- **D. Follow Stripe on credit notes and restore nothing.** Rejected: that
  takes credit away from the customer, a new rule this fix should not add.

## Consequences

### Positive

- A void or a credit note no longer turns expiring or promotional credit
  into never-expiring paid credit.
- The ledger stays balanced: balance equals block capacity after every
  return, and no written-off grant gets headroom back (the
  `balance_exceeds_block_capacity` and `expired_grant_headroom_reopened`
  doctor checks).

### Risks / open items

- **ADR-080's ratio-integrity line changes.** It said no writer shrinks
  `consumed_cents` on a commit grant. A return now does, by at most what an
  invoice drew from it. Consumed never falls below `cn_retired_cents`
  (relief retires only remaining credit), so relief cash stays bounded by
  what was paid.
- **Rounding and legacy draws on the paid branch.** The drawn share counts
  only attributed draws, so on an invoice with draws from before 2026-07-05
  more of the note is new credit than the invoice's payment implies. With
  legacy draws, refunds plus new credit can also exceed the cash paid; that
  was true before ADR-132 too.
- **A refused return blocks its writer.** A void or credit note whose
  attribution does not add up fails loudly instead of minting credit; an
  operator has to look at the ledger. No such ledger is known.
- **Restore order within one second.** Usage-row ids are xids, ordered by
  creation second; two rows on one invoice from two processes in the same
  second restore in id order.
- **A grant past expiry can have two expiry entries** (the sweep's
  remainder, then the returned share). Every reader sums them.
- **Not in scope.** The credit-note audit row is unchanged: the ledger rows
  show what expired or was retired. The dashboard needs no change: a return
  row's restored part no longer reads as used.

## References

- ADR-061: credit-note issue, and its open item "what kind of credit comes
  back".
- ADR-071: expiry retires the block, and its accepted residual.
- ADR-078: prepaid commits, and its listed limitation.
- ADR-080: commit relief, and its ratio-integrity line.
- ADR-129: credit carries its own currency.
- Migrations 0093 (credit-note dedup index), 0106 (reversal dedup index).
  No new migration.
- `internal/credit/return.go` (`ReturnCreditTx`, `planRestore`,
  `paidDrawnShare`, `spendableAt`, `classifyRestore`),
  `internal/credit/postgres.go`
  (`expireGrantRemainderTx`, `retireCommitSliceTx`, `ListGrantSummaries`,
  `balanceGrantedUsedColumns`).
