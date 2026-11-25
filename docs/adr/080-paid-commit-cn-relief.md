# ADR-080: Paid-commit credit-note relief (ADR-078 phase 2)

Date: 2026-07-06
Status: Accepted
Supplements: ADR-078 (prepaid commits phase 1), ADR-061 (atomic CN issue)
Design basis: docs/dev/design/commit-cn-relief.md (3-lens divergent panel +
adversarial judge; the cap arithmetic was mechanically verified against
`internal/platform/money/round.go` before acceptance).

## Summary

A paid prepaid-commit invoice (credits bought up front, sometimes at a discount) can be refunded partly or fully with a relief credit note: a `commit_relief` block on `POST /v1/credit-notes`. The operator names how many credits to retire, and the cash follows the price paid per credit, so a $9,000 commit worth $10,000 refunds $5,400 for $6,000 unused. One function, `commitReliefCash`, computes that cash cumulatively, so any series of partial refunds adds up to no more than was paid. The note is created and issued, and the credits retired, in one transaction; only the Stripe refund and the tax reversal run after commit, with retry. Status: active; ordinary credit notes on a commit invoice are refused and point to relief, and relief never pays into the credit balance. Amended 2026-11-25: relief takes only unused credit, is refused on a disputed payment, and its card cap counts refunds made in Stripe. Amended again 2026-11-25: money returned outside Hardcount (a refund made in Stripe, a lost dispute) is a credit note that takes back the credit it paid for, and relief refunds no more than the payment's documents leave.

## Context

ADR-078 shipped commit invoices as cash instruments with NO unwind for the
paid state: credit notes 409'd on any commit invoice, and only the UNPAID
path had relief (void retires the grant). Migration month is hand-built
commit invoices paid via `RecordOfflinePayment` — a wrong amount, wrong
customer, or churn with unused commit hit a dead end.

The hard problem is the **discounted commit**: pay $9,000, granted $10,000.
Refunding remaining credits at face lets a customer draw at discount and
refund at face (over-refund); charging consumption at face under-refunds.

## Decision

1. **Telescoping price-ratio anchor.** `f(k) = RoundHalfToEven(GrossPaid × k, Granted)`
   with `K = cn_retired_cents` (cumulative credits retired by relief CNs,
   a real column on the grant row). A relief retiring `r` credits refunds
   **`X = f(K+r) − f(K)`** gross. Worked example (9000 paid / 10000
   granted / 4000 consumed): max refundable = f(10000) − f(4000)… i.e.
   with K=0 and r=remaining 6000, X = **5400** — not 6000 (face), not
   5000 (face-minus-face). Telescoping makes ANY partial sequence sum to
   exactly `f(K_final) ≤ GrossPaid`; per-slice independent rounding is
   forbidden (it over-refunds: GrossPaid=2, Granted=3, three 1-credit
   slices → naive pays 3). **The anchor has exactly one call site
   computing CN cash** (`commitReliefCash`) — a second site reintroduces
   the rounding bug.
2. **Single coordinator tx, create-and-issue.** The relief CN is created,
   flipped issued, and the credits retired in ONE tx
   (`CreateAndIssueCommitRelief`): crash pre-commit leaves nothing;
   post-commit only the external legs (Stripe refund / tax reversal)
   remain, both idempotency-keyed and sweep-recovered — credits are
   retired at-or-before cash leaves, never refunded-but-drawable.
   Deliberately NOT the ADR-061 draft+reconciler shape: this is
   operator-initiated and idempotency-keyed at the HTTP layer, and a
   draft would neither reserve the document cap nor survive Issue()'s
   credit-channel re-derive default (the requirement-f hazard). Do not
   "fix" it into a draft flow.
3. **Partials in CREDITS, cash always derived.** `retire_cents` (int) or
   `retire_all` — which resolves to the LIVE remaining inside the tx, so
   a racing drawdown shrinks the relief instead of failing it; an
   explicit over-ask fails typed with the live numbers (never a silent
   clamp of an approved money document). Cash-denominated input may ship
   later as pure UI sugar.
4. **The credit channel is structurally forbidden** on relief (allocation
   = refund and/or out_of_band only): paying relief from the customer's
   balance would refund the very block being retired. Offline-paid
   commits (the `out_of_band:` PI marker) default to the out_of_band
   channel — the CN records the obligation; cash moves outside.
   *(Amended 2026-10-23, #1370: an offline payment now sets
   `paid_out_of_band` and records no PaymentIntent. The default reads
   "no PaymentIntent", which also covers it.)*
5. **Retire primitive single-sourced.** `retireCommitSliceTx` is the one
   retirement core; the void leg (slice = remaining, silent no-op on a
   missing grant, no K bump, `commit_void_retire`) and the relief leg
   (explicit slice, LOUD on a missing grant, K bump,
   `commit_refund_retire`) are thin wrappers — the complete
   commit-retirement writer set.
6. **Gates.** Unpaid commit → void (unchanged). Ordinary line-based CNs
   and `POST /invoices/{id}/refund` stay 409 on commit invoices with
   routing messages (cash ≠ face makes line-cap semantics wrong). Mixed
   commit+other-line invoices → 409 (trigger: first DP mixed-invoice
   refund ask). Expired grants → 409: term lapse is contractual breakage,
   earned cash; post-expiry goodwill is a promotional grant or
   out-of-band cash, never a cash CN. A zero-cash slice (deep-discount
   rounding) → 409 rather than a silent credit donation.
   *(Amended 2026-11-25: expiry is judged at the request's instant by the
   drawdown's liveness rule, live at `expires_at` and expired a moment
   later, whether or not the sweep has run; see the amendment below.)*
7. **Cap basis.** `GrossPaid = invoice.TotalAmountCents` — full-settle
   holds because Hardcount has no partial payments and commit invoices cannot
   be credit-paid. If partial capture ever ships, switch to captured cash.
8. **Wrong-price runbook.** Ratio corrections are FULL relief + reissue a
   corrected commit invoice — every relief preserves GrossPaid/Granted by
   construction, so partial relief cannot fix a wrong ratio.

## Consequences

- `credit.commit_retired` outbound event (in-tx with the retire) +
  `commit_retired_cents` on the CN document; auditors reconcile
  `grant.cn_retired_cents == SUM(cn.commit_retired_cents)` and recompute
  every relief from `(GrossPaid, Granted, K)`.
- Known blind spot (accepted): Hardcount models no card disputes — a disputed
  commit PI leaves the invoice paid while cash is pulled; the card refund
  leg fails safe at Stripe (loud `refund_status=failed`), the out_of_band
  channel has no backstop. Trigger: first dispute-modeling ask.
  *(2026-10-19, #979: the trigger fired. Disputes are now recorded. A lost
  dispute on a paid commit invoice raises `dispute_lost`, whose message
  states the still-spendable credit and names relief (`retire_all`, the
  amount as `out_of_band_amount_cents`), since an ordinary credit note is
  refused here. Hardcount still retires nothing by itself: a loss does not tell
  it whether the customer owes the money again, so the operator decides, as
  for every other lost dispute. Relief returns at most f(remaining), so a
  loss on a commit with spent credit leaves the rest to the operator's own
  books. Still open: nothing stops a relief from also refunding to the card
  after a loss; Stripe refuses that refund (`charge_disputed`), and it
  shows as a failed refund.)*
- Dashboard relief dialog is a fast-follow; the API carries the full
  contract (grants burndown endpoint reports remaining; error messages
  carry live max-refundable figures).
- Ratio-integrity invariant (no writer grows `amount_cents` or shrinks
  `consumed_cents` on a commit grant) holds today by construction and is
  pinned by tests, not a speculative gate. *(Changed 2026-10-30 by
  [ADR-132](132-returned-credit-goes-back-to-its-grants.md): a void or
  credit note returning drawn commit credit lowers `consumed_cents` by what
  an invoice drew. It never goes below `cn_retired_cents`, since relief
  retires only remaining credit, so relief cash stays at most what was paid.
  `amount_cents` still never grows.)*

## Amendment 2026-11-25 — relief takes only unused credit

- **Unused credit.** A relief takes at most the smaller of what is left of
  the grant at the request's instant and the customer's spendable balance in
  its currency less the usage not invoiced yet (`unusedCredit`). The grant is
  live at `expires_at` and expired a moment later, by the predicate
  `drainPositiveBlocks` uses, read in the grant's `FOR UPDATE` re-read. The
  spendable balance is read on the relief's transaction under the customer's
  credit lock; the unbilled usage, billing's figure, before the transaction.
  `retire_all` takes all of it; a larger `retire_cents` answers 422 with it.
  While an issued period or threshold invoice is still to draw on the grant
  (`domain.CreditDrawPendingInvoiceSQL`, built from the same rule as the
  invoice half of the expiry sweep's owed draw), relief is refused: that
  invoice's usage is no longer unbilled, and the credit it will draw is still
  in the balance. The refusal names the oldest such invoice and its way out:
  finalize or void a draft, resume a paused collection or void the invoice,
  or try again after the next collection pass.
- **A top-up only.** Relief requires `billing_reason = 'topup'`, which
  replaces the mixed-invoice gate of decision 6: a top-up has one line.
- **The card cap counts refunds made in Stripe** of the same payment that
  moved or may move money and that no note holds, read under the
  per-invoice credit-note lock (decision 4's PM refund cap).
- **Disputes.** Relief is refused, with code `payment_disputed`, while a
  chargeback is recorded on the invoice's own payment (an appeal included)
  and once the bank won it. When one opens during the relief, Stripe refuses
  the card refund, and "returned another way"
  (`POST /v1/credit-notes/{id}/mark-returned-another-way`) ends it. That
  action is refused while the chargeback is open, and allowed once it is
  lost. This replaces the 2026-10-19 note's advice to relieve a lost dispute
  out of band.
- **Retry and "returned another way".** Retry refund moves a failed refund
  to `pending` before it calls Stripe, and the mark needs it `failed`, so
  the two never both land.
- **Lock order.** The credit-note lock, the invoice row `FOR KEY SHARE`, the
  customer's credit lock and the grant row, then the tenant's number row.
  The early `FOR KEY SHARE` keeps the invoice row before the number row. The
  dispute recorder takes the invoice row `FOR NO KEY UPDATE`, so neither
  waits for the other.

## Amendment 2026-11-25 (2) — money returned outside Hardcount takes the credit back

- **One rule.** Money that goes back to the customer outside Hardcount on a
  top-up's own payment is recorded as it is on any invoice: one credit note
  for the amount. On a top-up the note also takes back the unused credit the
  amount paid for.
  - **A refund made in Stripe** is ADR-134's recorded note (its card refund
    is that refund). The prepaid-commit check of ADR-134 decision 4 is gone.
    Each refund stands alone, at its own amount.
  - **A lost dispute** is recorded in the dispute recorder's
    transaction, on the move into lost, while `invoice_disputes
    .credit_note_id` is NULL: one note for the smaller of the disputed amount
    and what the documents allow, money returned out of band ("Payment
    reversed in a dispute"), not emailed. A dispute in another currency writes
    none. `POST /v1/invoices/{id}/disputes/{dispute_id}/record-loss` runs the
    same step.
- **The credit a note takes back** is r = min(unused, the largest r with
  f(K+r) − f(K) ≤ the note's total), clamped to what the documents still
  allow. A note that uses up what the documents allow takes all the unused
  credit, whatever f says, so no credit outlives a payment returned in full.
  Every note's r bumps K, and f still reads K = `cn_retired_cents` alone.
  So Σ note totals = f(K) holds for operator reliefs only; a note for money
  returned outside Hardcount takes credit at its own amount.
- **One room for documents** (`documentsRoom`): what was paid, less the
  totals of the notes not voided, less refunds made in Stripe of the payment
  that no note holds (they returned money too). Relief, both notes for money
  returned outside Hardcount and the lost notice read it, under the
  credit-note lock.
- **Relief refunds** the smaller of the unused credit and the worth of that
  room. With nothing left it answers "This payment has already been returned
  in full." A `retire_cents` whose cash is the whole room answers `422`
  asking for `retire_all`, which takes all the unused credit. It waits while
  a refund made in Stripe on the invoice is not settled (ADR-134 decision
  11), and while a lost dispute on the payment has no note and the room is
  not spent (`dispute_loss_unrecorded`): record-loss writes it first. After
  a loss nothing goes to the card, and relief says to record the rest as out
  of band when it was paid back another way.
- **One card-refundable figure** (`domain.CardRefundsSQL`) for relief, an
  ordinary note's create, the Stripe-refund recorder and the doctor check:
  what the card paid, less the card refunds of notes not voided (failed ones
  too), less refunds made in Stripe that no note holds and that moved or may
  move money. It is arithmetic only. A lost dispute is a guard on the paths
  that send a refund (relief's card leg, an ordinary note's card refund,
  `POST /v1/invoices/{id}/refund`, Retry refund), not on the recorder or the
  linked create, which record a refund already made.
- **Lock order of every note on a top-up:** relief takes the credit-note
  lock, then the invoice row FOR KEY SHARE. The recorders (the Stripe-refund recorder, the dispute recorder, record-loss)
  and the linked create take the invoice row FOR NO KEY UPDATE first (and
  the dispute recorder the dispute row FOR UPDATE), then the credit-note
  lock; FOR NO KEY UPDATE does not conflict with FOR KEY SHARE, so the two
  orders do not deadlock. Then the customer's credit lock and the grant
  row, then the tenant's number row and the insert. The store numbers the
  note only after build returns (`CreateUnderInvoiceLockDynamicTx`), so the
  number row cannot come before the customer's lock, which would deadlock
  with the payment of the customer's other top-up.
- **The recording never waits on credit.** The usage not invoiced yet is read
  before the transaction. When it cannot be read, a refund is flagged
  `credit_note_not_written` and a dispute is recorded with no note; a
  Critical notice says why, and the linked create or record-loss writes it.
