# ADR-061: Credit-note `Issue()` atomicity (atomic internal effects + transactional-outbox external effects)

## Status

**Built — PR2 (2026-06-25).** See the **Amendment (2026-06-25)** at the end of
this ADR, which records where the implementation diverged from this blueprint.
Original status: Accepted (design-only) — 2026-06-24, build deferred to a named
trigger. The course changed (the user prioritised the end-to-end long-term fix
over deferral) and it shipped.

## Summary

`creditnote.Service.Issue()` commits the `draft` to `issued` status change and the internal money effect in one database transaction: a credit grant for a paid invoice, or an `amount_due` reduction for an unpaid one. If the effect fails, both roll back and the note stays `draft`, so a customer is never shown a credit they did not receive. The external effects (Stripe refund, tax reversal at the provider) run after commit with idempotency keys. Amended 2026-06-25: tax reversal does not use `webhook_outbox`, because its rows would reach customer endpoints. A scheduler sweep, `RetryPendingCreditNoteTaxReversal`, retries failed reversals with the same key.

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

### Not decided here, with triggers

- **Sizing still caps at amount due.** Credits applied before a note is sized are not returned, while credits applied after are. ADR-050 states the first as policy. The owner's decision (2026-10-12) is to return applied credits in general; that moves the four amount-due caps to a cap on the invoice total, in a follow-up change.
- **A charge already sent to the card network.** It is marked only by the charge lease, which `Issue()` does not read. Trigger: the first payment recorded with an amount that differs from what the card was charged.
- **What kind of credit comes back.** It is a plain grant with no expiry and no promotional kind, as a void's hand-back already is. Trigger: the first tenant with expiring or promotional credits applied to an invoice that is then credited or voided.

