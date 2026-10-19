# ADR-078: Prepaid commits + drawdown, phase 1

**Status:** Accepted (2026-07-05; amended 2026-08-04 and 2026-10-19; supplemented by [ADR-080](080-paid-commit-cn-relief.md) on 2026-07-06)
**Context:** `docs/design/prepaid-commits.md` (the locked design — peer-swept
across Metronome/Lago/Stripe/AI-infra with adversarial quote-verification,
then a 6-lens adversarial design panel; this ADR records the decisions, the
design doc holds the full evidence and census).

## Summary

A prepaid commit is credit a customer buys on a manual invoice and then draws
down. Velox grants it as a `customer_credit_ledger` block with
`grant_kind='commit'` in the same transaction that finalizes the invoice, so
the credit is spendable from the day the invoice is issued. Voiding the
invoice retires the unused balance, but uncollectible, dunning pause and
cancel do not. Customer credit never pays a commit invoice, ordinary credit notes are
refused on it, and drawdown spends promotional credit first. Unused credit on a
paid commit is refunded at the price paid through a relief credit note
([ADR-080](080-paid-commit-cn-relief.md)). Amended
2026-08-04: the attention dashboard flags an unpaid commit invoice whose
credit is still live (`commit_exposure`), and holding credit until payment
stays deferred until a self-serve or auto-top-up build exists. Amended
2026-10-19: the purchase of a commit is not taxed; the usage the credit pays
for is taxed when it is invoiced, so tax is charged once.

## Decision

Ship the prepaid-commit primitive on the existing credit-block ledger:

1. **Discriminator, not a new entry type.** `grant_kind`
   (`'commit' | 'promotional' | NULL`) + `source_invoice_id` on
   `customer_credit_ledger` (migration 0136), with a partial-unique
   fund-once index predicated on `grant_kind='commit'`. Legacy `NULL`
   grants are money-derived liabilities and drain in the **paid class**;
   `promotional` is explicit operator input only. Zero behavior change
   until a tenant uses either.
2. **Commit line = dedicated nullable columns** on `invoice_line_items`
   (`commit_granted_cents`, `commit_expires_at`) — not metadata JSONB
   (unvalidatable money config), not a side table (orphan lifecycle).
   Granted amount is independent of the line price (discounted commits:
   pay $9k, get $10k). Manual invoices only, `add_on` lines only, at most
   one per invoice — line-local rules in `buildLineItem` (shared by BOTH
   line-creation paths), invoice-level rules re-asserted in the store
   under the invoice row lock.
3. **Fund on ISSUE, in the finalize tx.** `FinalizeWithDates` grants the
   block via an injected `CommitFunder` (satisfied by `credit.Service`)
   inside the finalize coordinator tx — status flip and grant are
   both-or-neither. A funder error fails finalize: operator-synchronous,
   loud, retryable. The **payment gate** (pending-until-paid) and the
   markPaid funder hook are **phase 2** (trigger: first DP
   pending-until-paid ask, or the self-serve/auto-top-up build which
   hard-defaults to gated). Grant-on-issue is the negotiated-B2B default
   — see the 2026-08-04 amendment for the verified peer evidence, which
   supports the decision but not the sentence originally written here.
4. **Retire on VOID only.** Voiding a commit funding invoice retires the
   grant's remaining balance in the void tx (`RetireCommitGrantForInvoiceTx`
   — ExpireGrantAtomic's locked shape on the caller's tx, entry stamped at
   void-time, `entry_type='adjustment'` with
   `metadata.reason='commit_void_retire'`). The `consumed_cents` CAS makes
   the legal uncollectible→void sequence retire exactly once. Consumed
   stays consumed. **Uncollectible / dunning pause / cancel do NOT
   retire** — the block stays live as a collections stance (consistent
   with the other dunning terminals), which also makes the
   uncollectible→paid recovery correct with no restore leg (voided→paid
   is rejected, so a retired grant can never be paid for).
5. **Commit invoices are cash instruments.**
   - Credit balance never pays a commit funding invoice —
     `ApplyToInvoiceAtomic` no-ops on invoices carrying a commit line
     (in-tx re-read; covers the auto-charge sweep and dunning retry —
     otherwise a card-less customer's commit settles with its own
     just-granted credits: revenue booked on zero cash).
   - Credit notes are **blocked** on commit invoices (typed error at CN
     create, shared core so operator + automated paths agree). Unwind:
     unpaid → void (retires); paid → phase-2 CN-retire leg (trigger:
     first DP commit-refund ask).
6. **Status-flip CAS (prerequisite prod fix).** `FinalizeWithDates` gains
   `AND status='draft'`; `updateStatusInTx` gains an in-SQL allowed-source
   set (finalized←draft; voided←draft/finalized/uncollectible, never paid;
   uncollectible←finalized). Service guards read stale snapshots — the
   moment money hooks ride these flips, the CAS is what prevents
   pay-vs-void retiring paid-for credits and void-vs-finalize minting a
   grant on an annulled invoice.
7. **One global lock order:** invoice-row → customer advisory → ledger
   rows. `ApplyToInvoiceAtomic` now locks the invoice row FIRST and
   re-reads status/amount_due in-tx (also fixing the stale-due race:
   a drain racing a settle no longer burns credits into a paid invoice),
   then takes the advisory lock; `AdjustAtomic` takes the advisory lock
   before its row locks. This removed a reachable 3-way deadlock cycle
   (drain: ledger→invoice; expiry: advisory→ledger; new hooks:
   invoice→advisory) and is what makes alert crossings well-ordered.
8. **Drawdown order:**
   `ORDER BY (grant_kind IS NOT DISTINCT FROM 'promotional') DESC,
   expires_at NULLS LAST, created_at, id` — promotional (zero-cost-basis)
   first, then the paid class by the existing tiebreak. `IS NOT DISTINCT
   FROM` is load-bearing: a bare `=` sorts NULL first under DESC,
   draining legacy money-derived credits before free promo (verified
   against live Postgres). Applies to both drainPositiveBlocks callers
   (drawdown AND clawback attribution). No operator priority field —
   Metronome/Lago/Stripe numeric priority is the named phase-2 escape
   hatch.
9. **Balance alerts** (green field; the 0083-deleted subsystem's
   replacement): `credit.balance_low` (per-tenant threshold,
   `tenant_settings.credit_balance_low_threshold_cents`),
   `credit.balance_depleted` (>0→0), `credit.balance_recovered` (0→>0 —
   kept as depleted's state-machine complement, not for parity).
   Crossings computed on SUM(amount_cents) before/after inside each
   ledger-writing tx via one shared helper at the 3 insert sites,
   enqueued on the same tx (transactional outbox; credit store gained a
   `SetOutboxEnqueuer` mirror of invoice's). Payloads carry
   `customer_id` + `balance_cents` (+ `threshold_cents`). Known lag:
   expired-but-unswept blocks cross when the sweep retires them.
10. **Expiry default: never.** `commit_expires_at` optional; term-aligned
    defaults arrive with recipe-carried commits (phase 2) — a manual
    invoice has no term to align to, and inventing one is the banned
    silent-fallback class.

## Consequences

- New API surface: `commit_granted_cents`/`commit_expires_at` on line
  items; `grant_kind` (promotional only) + `expires_at` on
  POST /v1/credits/grant; threshold on tenant settings; 3 outbound event
  types.
- `VELOX_BILLING_ALERTS_INTERVAL` phantom removed from compose (nothing
  read it).
- Accepted, documented limitations (phase-2 triggers in the design doc):
  ReverseForInvoice re-grants drawn credits as NULL-kind/never-expiring
  (pre-existing class, needs per-block attribution); reporting stays
  face-value-aggregate; uncollectible/pause/cancel leave commit blocks
  live; the $1M per-grant cap applies to commits (split larger commits).
- Deferred: payment gate + paid-hook, auto-recharge, CN-retire leg,
  recipe commits + term-aligned expiry, drain-priority override,
  per-customer thresholds, pending-block visibility, true-up.

## Amendment 2026-08-04 — exposure is surfaced; the payment gate stays deferred

Triggered by a live observation on VLX-000060: $96.52 invoiced, $0.00
collected, dunning since Jul 30, and **$100.00 of the customer's credit
fully spendable** — with nothing anywhere in the product saying so.

**1. D3's fast-follow is shipped.** `docs/design/prepaid-commits.md` listed
"surfacing unpaid-funded grants in the attention dashboard" as a fast-follow
candidate; it is now built. A `commit_exposure` reason + `void_invoice`
action report an unpaid invoice whose commit credit is still live, on
finalized AND uncollectible rows.

The classifier **folds** rather than competing: when a cause banner already
won, its reason, its actions and their order are preserved and the exposure
is appended to the message. Two reasons, both of which a naive version gets
wrong. A commit invoice is unpaid-with-live-credit from finalize until
payment — on Net-30 that is thirty days of the deal working as agreed, so a
reason that fired there would cry wolf on every commit sale. And
`ClassifyInvoiceAttention` returns the FIRST match, so winning the chain
would have replaced "the customer's card was declined" with a restatement of
the deal terms on exactly the invoices that most need the card fixed.

**2. The payment gate is NOT built, and this amendment records why** — so it
is not re-derived from scratch next time.

D3's own trigger (first DP pending-until-paid ask, or the self-serve /
auto-top-up build) **has not fired**. The peer evidence, now verified with
quotes rather than recollection, points the same way: Orb attaches
`require_successful_payment` to credit *increments and top-ups*, and
Metronome attaches payment-gating to *"threshold billing and auto-recharge"*
while documenting full-term access — "the entire committed amount is
available on day one" — as typical for negotiated prepaid. Velox has neither
self-serve nor auto-recharge. Building the gate now is building for a
customer shape that does not exist.

**A design finding worth keeping.** The obvious implementation — a
`funding_state` column on the grant row, filtered in the drawdown and
flipped at the paid chokepoint — is a **denormalized copy of a fact the
database already holds**: `invoices.status` of the grant's own
`source_invoice_id`, which is already indexed and unique
(`idx_credit_ledger_commit_fund_dedup`). A join answers "is this spendable?"
with no column, no flip, no chokepoint, no migration — and unlike a flag it
cannot fail open on a missing INSERT column, cannot be left stale by a
settle path that bypasses the chokepoint, and needs no self-heal. An
adversarial pass over the flag design produced 21 findings, four of them
existing *only* because the truth would be copied instead of read. So the
"we will need the column eventually" argument is false. **If the gate is
ever built, start from the join.**

The one argument the join does not answer is per-line gating independent of
the invoice's status — which is exactly what auto-top-up will want. That is
the trigger: **build the gate when the self-serve / auto-top-up build lands,
and only then decide join-vs-flag on that build's requirements.**

**3. D2's peer-parity sentence was wrong and is corrected.** It read "Orb
`require_successful_payment` defaults off". Orb documents the flag as
something you *pass* to enable the gate and **states no default**; the
inference is reasonable but it was written as a citation. Verified 2026-08-04:

> "If successful payment is a requirement for credits to be effective, then
> pass `invoice_settings.require_successful_payment` when creating a credit
> increment… the credit block will be initialized with a status of
> `pending_payment` until the relevant invoice is paid. Although the block
> will be returned when fetching the customer credit balance, it will not be
> eligible to be drawn from until it's in `status=active`."
> — https://docs.withorb.com/product-catalog/prepurchase

Metronome, same date: access schedules are separate from invoice schedules;
"full-term access: the entire committed amount is available on day one —
typical for prepaid deals", with payment-gating offered for threshold
billing and auto-recharge —
https://docs.metronome.com/launch-guides/prepaid-credits/

**The at-issue default stands**, on the stronger of the two grounds it could
have rested on: for negotiated Net-30 B2B the invoice is the commercial
event and the customer expects to draw on day one. The peer evidence now
corroborates that rather than being asked to carry it.

## Amendment 2026-10-19 — the purchase is not taxed; the usage it pays for is

Issue [#1271](https://github.com/getvelox/velox/issues/1271). With a tax
provider on, a prepaid commit was taxed twice. The commit line was taxed on
the purchase invoice like any other line. Later usage invoices were taxed
again, and the credit paid their `amount_due`, which includes that tax. At
20% tax, a customer paid $12,000 for $10,000 of credit, and each $1,000 of
usage then drew $1,200 of it, so the credit covered about $8,333 of usage.
The account reported tax on about $18,333 (the $10,000 purchase and the
$8,333 of usage) for $8,333 of service.

**Decision: the Lago model.** Buying credit is an advance payment, not a
sale of a service. The commit line is left out of the tax request. It
carries tax 0 and `tax_reason = 'prepaid_credit_purchase'`, and the PDF says
why. Under the `none` provider the account collects no tax, so the line
carries no reason and the invoice says nothing about it. The usage is taxed
when it is invoiced, and the credit pays that invoice with its tax, as
before. Lago, verified 2026-10-19:

> "Taxes do not apply to credit purchases, which are considered as advance
> payments."
> — https://getlago.com/docs/guide/wallet-and-prepaid-credits/wallet-top-up-and-void

> "They are deducted from the post-tax subtotal, after any credit notes have
> been applied."
> — https://getlago.com/docs/guide/wallet-and-prepaid-credits/overview

**Why not Stripe's model.** The other option was to tax the purchase and
apply credit before tax, as Stripe applies billing credits: "Credit grants
apply to invoices after discounts, but before taxes" ([billing
credits](https://docs.stripe.com/billing/subscriptions/usage-based/billing-credits),
verified 2026-10-19). In Velox, credit is a cash offset applied after tax:
it pays `min(amount_due, balance)`, and `amount_due` includes tax. Applying
it before tax would change every invoice that credit pays, not only commit
invoices. Lago's model fits the ledger as it is and changes one place.

**Where it is enforced.** `collapseTaxRequestLines`
(`internal/billing/tax_lines.go`) builds every tax request, for every path:
the operator's finalize of a manual invoice, the retry family (operator
retry, the tax-retry reconciler), and the engine's cycle, threshold, cancel
and proration invoices. Commit lines exist only on manual invoices, so in
practice the finalize and retry paths are the ones that see them.
`ApplyTaxToLineItems` stamps the commit line untaxed. An invoice with only a
commit line does not call the provider at all. An invoice with a commit line
is taxed from the sum of its lines, not its stored subtotal
(`commitInvoiceSubtotal`). A tax-inclusive draft computed before this change
stored its subtotal with the commit line carved to net, while its lines kept
the price entered; finalized now, it is invoiced at that price.

**Consequences.**

- A commit invoiced after this change has no tax calculation or Stripe Tax
  transaction for its commit line. The usage it pays for is reported when
  invoiced.
- Refunding unused credit through relief (ADR-080) reverses no tax on these
  commits: relief derives its tax from the purchase invoice's own tax, which
  is 0. A commit invoiced before this change carried tax, and relief still
  reverses its share.
- Voiding an unpaid commit-only invoice has no tax transaction to reverse.
  A mixed invoice under Stripe Tax still has a transaction for its other
  lines, and voiding reverses it as before.
- The credit pays each usage invoice with its tax, so with tax on it covers
  less usage than its face value. At 20%, $10,000 of credit pays for about
  $8,333 of usage and $1,667 of tax. That was already so. What changed is
  that the purchase is no longer taxed as well.
- On a discounted commit the usage is taxed at its list price, which is
  more than the customer paid for the credit. At 10%, $10,000 of credit sold
  for $9,000 pays for about $9,091 of usage, and all of it is taxed. Some
  voucher rules tax the use of credit on the price paid for it instead. One
  to check is the EU rule for multi-purpose vouchers (Article 73a of
  Directive 2006/112/EC; not quoted here). Trigger: an account
  sells discounted commits where the use of credit is taxed on the price
  paid.
- No discount reaches an invoice with a commit line today: nothing writes a
  discount on a manual invoice. So the tax request's discount is not split
  between the commit line and the taxed lines. Trigger: anything that lets a
  manual invoice carry a discount must split it first, or the whole discount
  lowers the taxed lines' base.
- Issued invoices are not changed.

**Tax rules for prepayments differ by country.** Some countries tax a
prepayment when it is received. Others tax it when the service is used, or
treat credit usable for several services as a voucher taxed on use. An
account should check with its tax adviser:

- whether its country taxes prepaid credit when it is bought or when it is
  used;
- whether that depends on what the credit can be spent on;
- if it sells credit below face value, whether the use of the credit is
  taxed on the price paid or on the usage (Velox taxes the usage);
- what its invoices must say about an untaxed prepayment.

Velox has no setting to tax the purchase instead. Trigger to build one: an
account whose country taxes prepaid credit when it is bought, selling
commits with tax on.

**Promotional credit is unchanged.** Free promotional credit also pays the
tax on the usage it covers. Whether it should apply before tax is a separate
decision. Trigger: a partner uses promotional credit with tax on.
