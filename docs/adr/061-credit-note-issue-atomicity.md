# ADR-061: Credit-note `Issue()` atomicity (atomic internal effects + transactional-outbox external effects)

## Status

**Built — PR2 (2026-06-25).** See the **Amendment (2026-06-25)** at the end of
this ADR, which records where the implementation diverged from this blueprint.
Original status: Accepted (design-only) — 2026-06-24, build deferred to a named
trigger. The course changed (the user prioritised the end-to-end long-term fix
over deferral) and it shipped.

## Summary

`creditnote.Service.Issue()` commits the `draft` to `issued` status change and the internal money effect in one database transaction: a credit grant for a paid invoice, or an `amount_due` reduction for an unpaid one. If the effect fails, both roll back and the note stays `draft`, so a customer is never shown a credit they did not receive. The external effects (Stripe refund, tax reversal at the provider) run after commit with idempotency keys. Amended 2026-06-25: tax reversal does not use `webhook_outbox`, because its rows would reach customer endpoints. A scheduler sweep, `RetryPendingCreditNoteTaxReversal`, retries failed reversals with the same key. Amended 2026-10-19: a note that leaves an unpaid invoice with nothing due marks it paid in the same transaction, unless a charge hold is set.

## Context

The liveness audit (2026-06-23) found a sink in `creditnote.Service.Issue()`:
the `draft→issued` compare-and-swap (CAS) commits in its **own**
transaction, and the side effects run **afterward** in separate calls. If a
side effect fails, the credit note is durably `issued` while its effect
never happened — most visibly, an `issued` credit-channel note whose
`GrantForCreditNote` failed leaves the customer **shown a credit they never
received**, with no automatic recovery (re-Issue refused: `status!=draft`;
Void refused: issued is final).

The first instinct was a reconciler that re-runs the missing grant. The
project principle (see `feedback_first_good_practice_default`) corrects
that: **go for the first good practice by default; the second practice
needs a really strong reason.** A reconciler is the *second* practice; it
over-applies eventual-consistency to a side effect that could simply be
atomic.

`Issue()` does three kinds of work, and the right tool differs by kind:

1. **State transition** — `draft→issued` (the CAS). Internal.
2. **Internal side effects** — `GrantForCreditNote` (credit ledger) for a
   paid invoice's credit note, or `ApplyCreditNote` (reduce `amount_due`)
   for an unpaid invoice. Both are DB writes on the same pool.
3. **External side effects** — the Stripe refund (refund-channel note) and
   the **tax reversal** at the provider. Network calls; they cannot share a
   DB transaction.

Today (1) is its own transaction and (2) and (3) are separate, unguarded
calls afterward — a classic dual-write.

## Decision

Make `Issue()` first-practice end-to-end, by kind of effect.

### Internal effects → one transaction (atomic)

The CAS and the internal side effect commit together in a **coordinator
`*sql.Tx`** threaded through the creditnote, credit, and invoice stores —
the ADR-056 pattern Velox already uses for proration. The CAS provides both
concurrency safety (exactly one caller proceeds) **and** atomicity: a grant
or `amount_due` failure rolls the issue back, the note stays `draft`, and
the caller retries cleanly. The issued-but-ungranted orphan **cannot
exist** — there is nothing to reconcile.

Shape:

- New tx-accepting store variants: `creditnote.TransitionStatusTx`,
  `credit.GrantForCreditNoteTx`, `invoice.ApplyCreditNoteTx` — each
  operating on a passed `*sql.Tx`.
- `Issue()` opens the coordinator tx (RLS: `BeginTx(ctx, TxTenant,
  tenantID)`), performs CAS + the internal effect on that tx, and commits.

### External effects → transactional outbox (first-practice for a network boundary)

The Stripe refund and tax reversal **cannot** be in the DB transaction —
that is the genuine strong reason, and it is the *only* place the second
tier is justified here. But the first practice for a network boundary is
**not** fire-and-forget + an after-the-fact reconciler scan; it is the
**transactional outbox** (ADR-040): enqueue the *obligation*
("reverse tax for invoice X", "refund PaymentIntent Y") **inside the
coordinator tx**, so the intent commits atomically with the issue and
**cannot be lost**. A dispatcher then executes it with Stripe idempotency
keys; a backstop sweep handles dead-letter rows.

So: the obligation is atomic; only its execution is asynchronous.

### Reconciler → backstop only

A reconciler/marker-scan survives in exactly one role: the dead-letter
sweep for stuck outbox rows. It is never the primary mechanism for a
correctness-bearing effect.

### Net invariant

After this change, for any committed `issued` credit note: its internal
effect is committed (atomic), and its external obligations are durably
enqueued (outbox). Neither an ungranted orphan nor a lost tax-reversal can
occur. The 0093 credit-ledger dedup index and Stripe idempotency keys
become belts, not the mechanism.

## Consequences

- One coherent flow, no special-case recovery paths for the internal grant.
- The external effects move from inline to enqueue-in-tx + async dispatch —
  a sequencing change that must preserve correctness (do not reverse tax or
  refund before the issue is durably committed; the outbox enqueue is what
  guarantees the ordering).
- Moderate refactor: three tx-variant store methods + the coordinator in
  `Issue()` + moving two external effects onto the outbox. The patterns
  (ADR-056 coordinator, ADR-040 outbox) already exist; this is composition,
  not invention.
- Tests: atomic-rollback (grant fails → note stays `draft`, no ledger
  entry); obligation-durability (Stripe down at issue → outbox row present,
  dispatched later, exactly once); no double-grant under concurrent Issue.

## Alternatives considered

- **Reconciler-first (re-run the grant via the 0093 dedup index).**
  Rejected as the *primary* — it is the second practice applied to an
  internal effect that can be atomic; it leaves a window and needs a
  scan/marker. Retained only as the deferred-build fallback if the atomic
  refactor itself is postponed.
- **A persisted `grant_settled` / `issue_pending` marker column to bound a
  reconciler scan.** Rejected — solves a scale problem we don't have and
  cements the second practice with a migration.
- **Outbox for the *internal* effects too.** Rejected — overkill; an
  in-process DB write belongs in the transaction, not on an async queue.
- **Do nothing.** The current state is the deferred gap; acceptable only
  because it is pre-launch with no victim (see Scope).

## Scope and sequencing

**Deferred build**, trigger = the first design partner issuing
account-credit credit notes (the feature carries real money). When built,
do it as **one pass covering the whole flow**, not piecemeal — and **fold
in the sibling gap**: `reverseInvoiceTax` can also fail post-issue today
(an external effect), which wants the identical outbox treatment. So the
unit of work is: internal effects (grant, `amount_due`) atomic in the
coordinator tx **+** external effects (tax reversal, refund) on the
transactional outbox — grant + reduce + reverse + refund, together.

Until then, a grant failure here is an operator-visible error plus a manual
fix; the marker comment at the grant site records this.

## Related

- `feedback_first_good_practice_default` — the principle this ADR applies.
- ADR-040 — transactional outbox (the external-effect first-practice tool).
- ADR-056 (#287) — coordinator `*sql.Tx` (the internal-effect first-practice
  tool, proration).
- ADR-057 (#290) — clawback CN created in the item-change tx; documented the
  post-CAS partial-issue gap this ADR closes generally.
- ADR-059 (#296) — in-flight clawback deferred via a reconciler citing the
  zero-cross-peer-import rule. **Open question for the broader design:**
  that rule is not a strong reason against an *outbox* (a shared generic
  table needs no cross-peer import), so the clawback path may warrant the
  same outbox treatment for consistency. Tracked by the product-wide
  first-vs-second-practice audit (2026-06-24).

## Amendment (2026-06-25): Built in PR2

`creditnote.Service.Issue()` now runs the `draft→issued` CAS and the **internal**
money effect — `amount_due` reduction (unpaid) or credit grant (paid) — on one
coordinator `*sql.Tx` (ADR-056), committing them together. The **external**
effects (Stripe refund, upstream tax reversal) run post-commit, idempotency-keyed
and recoverable. The implementation diverges from the blueprint above in four
recorded ways (per `feedback_amend_decisions_when_course_changes`):

1. **Coordinator-tx is the mechanism; NO source-dedup table.** `ApplyCreditNote`
   and `GrantForCreditNote` each have exactly **one** caller (`Issue()`), gated by
   the CAS, so the reduction/grant is idempotent **by construction** — a crash
   mid-tx rolls both back (note stays `draft`, reconciler re-drives cleanly); a
   crash post-commit leaves `status='issued'`, so a re-entry loses the CAS
   (`won=false`) and never re-applies. The blueprint's `(tenant, source)` dedup row
   is therefore dead weight today and would be belt-and-suspenders
   (`feedback_no_belt_and_suspenders`); it is recorded as the **first brick of the
   deferred `amount_due`-derived-ledger** (north star below). A code comment at
   each apply/grant site pins the single-CAS-gated-caller invariant — a reviewer
   adding a second caller MUST reintroduce the dedup.

2. **CN tax reversal: post-commit + durable marker + CN-scoped sweep — NOT on
   `webhook_outbox`.** The blueprint said "tax reversal on the transactional
   outbox." That outbox (ADR-040) is a **customer-notification fan-out**
   (`dispatcher → svc.Dispatch → matchesEvent` against `'*'`/prefix
   subscriptions), so an `internal.*` obligation row would **leak to customer
   endpoints**. Instead PR2 keeps the reversal post-commit with its existing per-CN
   `velox_tax_rev_<cn.ID>` key and makes it recoverable: a durable
   `credit_notes.tax_reversal_pending` marker (migration 0123), a CN-scoped
   scheduler sweep (`RetryPendingCreditNoteTaxReversal`) that re-drives with the
   same key, and the inline failure raised `WARN→ERROR`. The sweep's eligibility
   is **derived from durable structural state** (an issued CN with no reversal
   stamped against a tax-bearing `stripe_tax` source), with the marker as a
   fast-path index — **not** the sole key — so a compound failure (reversal *and*
   marker write both fail) is still recovered, mirroring #310 whose eligibility is
   the default persisted state (caught by the pre-merge review). This closes a
   **reachable live over-remit**: `#310 RetryPendingTaxReversal` scans only
   voided/uncollectible invoices and keys off `invoices.tax_reversed_at`, so a CN
   reversal stamped on a finalized/paid invoice is structurally invisible to it.
   The outbox-obligation route is the **north star**, un-deferred at the first live
   tax design partner (when a dispatcher event-type discriminator + obligation
   handler-registry is justified).

3. **Reconciliation is first-class.** Reconciliation against processor truth is
   mandatory even with an idempotent external API (chargebacks, async settlement
   the queue can never see). The "second practice" caution applies only to a
   reconciler used as the *primary* mechanism for an effect that **could** be made
   atomic — not to processor-truth reconciliation, nor to the CN tax-reversal sweep
   above (whose effect is genuinely external and cannot share the DB tx).

4. **Async backbone — DESIGN decided (ADR-062), BUILD deferred.** The four
   bespoke re-drive sweeps (tax-commit #267, tax-reversal #310, clawback-issue
   ADR-057, PR2's CN-tax-reversal) will eventually consolidate onto ONE durable
   obligation queue, built by **generalising the existing `webhook_outbox`** (it
   already has in-tx `Enqueue(ctx, tx, …)`, `FOR UPDATE SKIP LOCKED`, DLQ,
   advisory-lock leader) with an internal/external discriminator. Decided AGAINST
   Temporal (its enqueue is an RPC to a separate datastore → reintroduces the
   dual-write; a cluster/SaaS breaks the self-host wedge; workflows are the wrong
   abstraction for single-step idempotent jobs) and AGAINST River-for-now (its
   `InsertTx` needs a pgx-native `pgx.Tx`; Velox is on `database/sql`, so its in-tx
   enqueue wouldn't compose with our coordinator `*sql.Tx` without migrating the
   whole data layer — 60 files / 385 query sites / the RLS core — *premise false
   since River v0.10.0; corrected in ADR-062's 2026-08-30 Amendment, decision
   unchanged*). At low/mid scale
   the four sweeps are correct and the consolidation is a *maintainability* win,
   not a correctness need, so the **build is trigger-gated** (≥~6 async effects, a
   non-notification obligation a reconciler can't cover, or a worker-process
   split). Full build-vs-buy + pgx-now-vs-later rationale and triggers in ADR-062.

### North stars / do-not-build (recorded, with triggers)

- **Generic durable obligation queue** (PR3 above) — trigger: a durable async
  obligation that is not webhook-notification-shaped, or a `cmd/velox-worker`
  process split (ADR-040 anticipates it).
- **`amount_due` as a derived ledger** (+ its `(tenant, source)` dedup first
  brick) — trigger: a **second** independent `amount_due` mutator (e.g.
  out-of-band partial payments racing a credit note), at which point the
  single-CAS-gated-caller invariant no longer covers all writers.
- **`EventDispatcher.Dispatch` `*sql.Tx` threading** across the ~8 fire-and-forget
  webhook emitters — trigger: first observed lost-event incident, or a design
  partner reconciling off webhook delivery. A notification seam (a lost event
  self-heals via subscriber re-poll), not a money-state seam.

### What shipped

Migration 0123 (`credit_notes.tax_reversal_pending` + partial index); tx-variant
store methods (`TransitionStatusTx`, `UpdateAllocationTx`, `ApplyCreditNoteTx`,
`GrantForCreditNoteTx`, `creditnote` `BeginTx`); the `Issue()` coordinator-tx
rewrite (both paid and unpaid branches); `RetryPendingCreditNoteTaxReversal` +
its scheduler tick; the `WARN→ERROR` raise; deletion of the now-false
"manual reconcile" comment in `engine.IssueCancelDrafts`. Tests: a real-Postgres
grant-failure-rolls-back-the-CAS proof, an in-memory failed-reversal-marks-pending
+ sweep-recovers proof, and the existing CAS-idempotency suite (unchanged, still
green). No new reconciler key beyond the marker; reuses ADR-056/057 primitives.

## Amendment (2026-10-12): issuing never relieves more than the invoice total

### What was wrong

A draft credit note is sized against the invoice's amount due when it is created. The amount due can fall before the draft is issued, because customer credits can be applied to the invoice in between. `ApplyCreditNoteTx` hid this with `GREATEST(amount_due - note, 0)`.

Reproduced on real Postgres with no concurrency:

| Step | Amount due | State |
|---|---|---|
| Invoice finalized, unpaid | 100.00 | total 100.00 |
| Draft credit note created | 100.00 | draft for 100.00 |
| Customer credits applied | 40.00 | 60.00 of credits consumed |
| Draft issued | 0.00 | credit note 100.00 issued, no error |

The 100.00 invoice was relieved by 160.00, and the customer's 60.00 of credits bought nothing. It is reachable through the API (a draft and a separate issue call) and through a cancel's relief draft held on a draft invoice, because finalizing that invoice applies the customer's credits.

### Decision

On the unpaid branch, `Issue()` now does this inside its coordinator transaction:

1. It locks the invoice row (`LockForCreditNoteTx`, `FOR NO KEY UPDATE`) before the `draft` to `issued` change, and decides everything from that row: whether to hold the note, which branch applies, and how much is still due. The read taken before the transaction only spares a transaction when the note plainly cannot issue yet.
2. It reduces amount due by `min(note total, amount due)`.
3. The rest of the note relieves the part that applied credits had covered. That many credits come off the invoice (`credits_applied_cents` goes down) and are granted back to the customer's balance in the same transaction, through the existing per-credit-note grant.
4. `ApplyCreditNoteTx` subtracts exactly. A note that asks for more than is due, or to return more credits than are applied, is refused with `credit_note_exceeds_invoice` and the note stays a draft.

The note itself is unchanged: its total, tax and three allocation channels are as created. A note against an unpaid invoice still has all three channels at zero.

### Why the returned credits are recorded on the invoice, not on the note

The alternative was to store the returned amount as the note's `credit_amount_cents`. That gives one field two meanings. Three readers treat a note as either "reduces the invoice" or "money back", and a mixed note breaks each: the `cn_allocation_split_all_or_nothing` doctor check, the invoice PDF totals, and the dashboard totals. Lowering `credits_applied_cents` gives the same money result and those readers need no change. The cost is that the credit-note document does not mention the balance return; the credit ledger entry and the `credit_note.issued` audit row (`amount_due_reduced_cents`, `credits_returned_cents`) do.

### What else had to change

- **Voiding the invoice afterwards.** The void hand-back summed every credit-usage entry for the invoice, so it would have returned the same credits a second time. It now subtracts what credit notes already returned (`SumCreditNoteGrantsForInvoice`). This relies on one fact: a credit-note grant on an invoice that can still be voided is always such a return, because the other kind is written for a paid invoice and a paid invoice cannot be voided.
- **The doctor.** `invoice_due_conservation` no longer skips invoices that have credit notes. It counts the part of issued notes that reduced the invoice: a note's total minus its three channels. It also flags any invoice whose paid amount, applied credits and credit notes exceed its total. That second condition is needed: on a row the old clamp damaged, amount due is 0 and equals the clamped formula.

### What did not change

- `create()` takes no invoice row lock. Its amount-due cap is an early refusal only: a draft reserves nothing, so two drafts that each fit can together exceed what is due. `Issue()` is where a note is fitted to the invoice. A row lock in `create()` would also invert the lock order of transactions that draft several notes (they take the per-invoice advisory locks first).
- No backfill. The widened doctor check lists any invoice the old clamp over-relieved; the remedy is a manual credit grant for the excess.

### Sizing: a note may cover applied credits (same day, second change)

The change above returned credits applied *after* a note was sized. Credits applied *before* were still dropped, because every place that sizes a note against an unpaid invoice capped it at amount due. The owner's decision (2026-10-12) is that a credit note returns applied credits in both orders.

The cap is now `Invoice.UnpaidRelievableCents()`: amount due plus the customer credits applied to the invoice. It replaces amount due at five sites:

- `creditnote` create, for a finalized unpaid invoice;
- `unpaidRelief` and the allocator's unpaid cap in the billing engine (cancel and swap relief);
- the two downgrade paths in the subscription handler (one source invoice, and the split across several).

Example: a 49.00 prebill with 30.00 of credits applied (19.00 due), cancelled halfway. The unused half is 24.50. Before, relief stopped at the 19.00 due and 5.50 of the customer's credits stayed spent on unused time. Now the note is 24.50: it clears the 19.00 and returns 5.50.

A fully unused unpaid invoice is still voided, which hands back all its credits. The cap on the sum of an invoice's credit notes (the invoice total) is unchanged.

Not changed: the dashboard's credit-note dialog still offers amount due as the maximum for an unpaid invoice. It is stricter than the API, not wrong; widening it is a UI change to review separately.

### Not decided here, with triggers

- **A charge already sent to the card network.** It is marked only by the charge lease. `Issue()` reads the lease only to decide whether to mark the invoice paid (amendment 2026-10-19), never before it lowers the amount due. Trigger: the first payment recorded with an amount that differs from what the card was charged.
- **What kind of credit comes back.** It is a plain grant with no expiry and no promotional kind, as a void's hand-back already is. Trigger: the first tenant with expiring or promotional credits applied to an invoice that is then credited or voided.

## Amendment (2026-10-19): a note that leaves nothing due marks the invoice paid

**Decision.** When a credit note on an unpaid invoice leaves nothing due, `Issue()` marks the invoice paid in its own transaction, with `invoice.paid` queued in that transaction. The one exception is a charge hold on the invoice (ADR-127): then the invoice is left open at $0. After the commit, `Issue()` closes the invoice's dunning run as `payment_recovered`, best-effort.

**Why.** The real trigger: an operator waives a dunned invoice (one whose card payment failed) with a credit note for what is due. Before, the invoice stayed finalized with its payment failed and nothing due:

- no charge path takes a $0 invoice, and **Collect payment** refuses one;
- the next dunning retry closed the run as recovered, with the invoice still unpaid and no `invoice.paid`;
- or exhaust-on-entry ran the final action anyway: it canceled or paused the subscription and wrote off a $0 invoice. A cancel cannot be undone.

Stripe does the same: a credit note that "reduces the balance of an open invoice to 0" makes the invoice paid. This is the ADR-066 rule for a zero-due invoice (paid with no payment attempt), applied at the one writer that could leave one open.

**How.**

- One store predicate, `invoice.PostgresStore.SettleZeroDueTx`, under the caller's row lock: the invoice is finalized, its payment is pending or failed, nothing is due, tax is `ok`, and no charge hold is set. Not settling returns the row, never an error, so the note still issues. The paid flip shares `markPaidLockedTx` with `MarkPaid`, so the checkout claims close and `invoice.paid` fires exactly as on every other settle.
- **Any hold refuses, its own owner's included.** Dunning's final action cancels the subscription under its hold, and the cancel's relief note can take a partly credit-paid invoice to $0. If the invoice turned paid in the middle of that, the write-off would fail on a paid invoice. The run would go to `action_failed` and close a day later as `payment_recovered`, and the canceled customer would get no escalation email. A stale hold refuses too: its charge may have taken the money.
- `Issue()` passes the invoice as it was locked, before the note, to its post-commit legs. The refund leg reads the payment status, and a note that marked the invoice paid must not look like a note on a paid invoice there.
- The dunning retry, on a $0 invoice, now calls `SettleZeroDue` (the same predicate in its own transaction) and closes the run only if the invoice is paid. Otherwise it skips the attempt, uncounted. This covers a credit balance applied by an earlier retry whose mark-paid failed.

**What this changes.** An invoice a credit note takes to $0 is paid, so it can no longer be voided. Stripe behaves the same. `invoice.paid` for it carries `amount_paid_cents` 0. A second note on the invoice then takes the paid-invoice path: its amount goes to the customer's balance. The customer gets back the same amount as before, and the credits applied stay recorded as what paid the invoice.

**Readers of a paid invoice (review of #1306).** A waived invoice is paid with nothing paid for it, so three readers that took "paid" to mean money came in changed with it:

- **Revenue.** The dashboard's Revenue and revenue chart summed the totals of paid invoices. A note on an unpaid invoice leaves the total as it was, so an invoice waived in full added its whole total. They now sum what paid each invoice: `amount_paid_cents + credits_applied_cents`. For an invoice paid in full with no such note the two are equal. An invoice lowered by a note and then paid now counts what was paid, which it did not before. A note on an invoice already paid changes neither figure: Revenue is before refunds.
- **The paid row.** The `credit_note.issued` audit row records `marked_invoice_paid`. When it is set, the invoice's paid row names that note and its issuer instead of "After N retries": the retries failed, and the note settled the invoice.
- **An item change after a full waive.** Removing an item, lowering a quantity or downgrading in the same period sends the paid invoice down the clawback path, and the waiver used all its credit headroom. That answered 500 ("no creditable funding invoice for period"). The period is already fully credited, so nothing more is owed and the change now proceeds, with an ERROR log. This is ADR-118's rule for a cancel or plan swap, applied to the item change. Before G3 the same change went through with no credit.

**Left as it is.** With a policy that cancels but does not write off, a dunning cancel whose relief note clears the invoice leaves it open at $0. No money is at stake. A note issued while a charge holds the invoice leaves it open at $0 too. If that charge fails, the next dunning retry marks it paid. Auto-charge does not, since it takes only an invoice whose payment is still pending. A cancel or plan swap after a full waive logs ADR-118's ERROR ("already fully credited"); it gives no relief, as before G3.

**Not done.** No $0 check in dunning's exhaust path, and no sweep for open $0 invoices. After this change the exhaust path meets one only through two coincidences. One is a note issued during a final charge. The other is a failover between the credit apply and the mark-paid on the last retry, which also loses the attempt's rewind.

**Waiting on the owner: a partial waive meets the paid-invoice headroom rule.** When customer credits paid part of an invoice and a note waived the rest, the invoice is paid with headroom equal to the credits. Two paths then treat a credit larger than that headroom as they treat a card-paid invoice partly credited before:

- a cancel or plan swap gives no relief at all, with an ERROR log (`allocateUnusedAcrossFunding` returns `errHeadroomExhausted` for an all-paid set). Before G3 the unpaid path returned up to the credits applied;
- an item change is refused with a 500 (`resolveClawbackPieces`). Before G3 it went through and returned up to the credits applied.

Example: credits paid 20.00 of a 100.00 invoice, the card failed on 80.00, the operator waived it, then canceled halfway. Before G3 the 20.00 of credits went back to the balance; now nothing does. Placing what fits and logging the rest would fix both, but it also changes those two paths for card-paid invoices, so it waits for a decision. Trigger: the first partial waive followed by a cancel, swap or item change in the same period.
