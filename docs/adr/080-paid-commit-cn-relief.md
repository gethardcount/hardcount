# ADR-080: Paid-commit credit-note relief (ADR-078 phase 2)

Date: 2026-07-06
Status: Accepted
Supplements: ADR-078 (prepaid commits phase 1), ADR-061 (atomic CN issue)
Design basis: docs/design/commit-cn-relief.md (3-lens divergent panel +
adversarial judge; the cap arithmetic was mechanically verified against
`internal/platform/money/round.go` before acceptance).

## Summary

A paid prepaid-commit invoice (credits bought up front, sometimes at a discount) can be refunded partly or fully with a relief credit note: a `commit_relief` block on `POST /v1/credit-notes`. The operator names how many credits to retire, and the cash follows the price paid per credit, so a $9,000 commit worth $10,000 refunds $5,400 for $6,000 unused. One function, `commitReliefCash`, computes that cash cumulatively, so any series of partial refunds adds up to no more than was paid. The note is created and issued, and the credits retired, in one transaction; only the Stripe refund and the tax reversal run after commit, with retry. Status: active; ordinary credit notes on a commit invoice are refused and point to relief, and relief never pays into the credit balance.

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
7. **Cap basis.** `GrossPaid = invoice.TotalAmountCents` — full-settle
   holds because Velox has no partial payments and commit invoices cannot
   be credit-paid. If partial capture ever ships, switch to captured cash.
8. **Wrong-price runbook.** Ratio corrections are FULL relief + reissue a
   corrected commit invoice — every relief preserves GrossPaid/Granted by
   construction, so partial relief cannot fix a wrong ratio.

## Consequences

- `credit.commit_retired` outbound event (in-tx with the retire) +
  `commit_retired_cents` on the CN document; auditors reconcile
  `grant.cn_retired_cents == SUM(cn.commit_retired_cents)` and recompute
  every relief from `(GrossPaid, Granted, K)`.
- Known blind spot (accepted): Velox models no card disputes — a disputed
  commit PI leaves the invoice paid while cash is pulled; the card refund
  leg fails safe at Stripe (loud `refund_status=failed`), the out_of_band
  channel has no backstop. Trigger: first dispute-modeling ask.
- Dashboard relief dialog is a fast-follow; the API carries the full
  contract (grants burndown endpoint reports remaining; error messages
  carry live max-refundable figures).
- Ratio-integrity invariant (no writer grows `amount_cents` or shrinks
  `consumed_cents` on a commit grant) holds today by construction and is
  pinned by tests, not a speculative gate.
