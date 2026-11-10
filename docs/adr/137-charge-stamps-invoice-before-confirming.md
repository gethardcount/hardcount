# ADR-137: A charge puts its PaymentIntent on the invoice before confirming it

**Date:** 2026-11-10
**Status:** Accepted
**Supersedes:** [ADR-107](107-unknown-is-terminal-until-a-human.md)'s parked
state and [ADR-127](127-a-lost-charge-result-is-resolved-by-asking-stripe.md)'s customer-list
check, for charges this version makes.
**Amends:** [ADR-001](001-paymentintent-only-stripe.md) (a charge is two
PaymentIntent calls), [ADR-105](105-charge-idempotency-key-seed.md) (the
counter moves only on PaymentIntent stamps and on the clear of a hold whose
charge confirmed nothing),
[ADR-117](117-one-payment-intent-per-invoice-deferred.md) (option C, below).

## Summary

Every off-session charge is three steps: create the PaymentIntent
unconfirmed, put it on the invoice as an unknown outcome, then confirm it.
The second step is one compare-and-set on the invoice row, and nothing is
confirmed unless it applied. So before any money can move the invoice itself
names the payment, and every way a confirm can end without an answer (a
crash, a timeout, a 500, a refused key) leaves a state Hardcount already
resolves: the unknown sweep reads that PaymentIntent by id, records its
payment, or cancels it for good and frees the invoice. Invoices are no longer
parked for 24 hours, the risk that a payment Stripe completes after that wait
is a second charge (ADR-127 R10) is gone, and the stale-hold sweep no longer
talks to Stripe at all. The charge hold still allows one attempt at a time;
this ADR does not replace it.

## Context

With create and confirm in one call (`confirm=true`), a 500, a 409, a network
error or a timed-out call named no PaymentIntent. Stripe keeps an idempotency
key for at least 24 hours and replays a saved 500 under it, and during incident
recovery it may still complete the request (Stripe,
[advanced error handling](https://docs.stripe.com/error-low-level)). So a
same-key retry returned the same error for a day, and the only other tool was
a scan of the customer's PaymentIntents. Hardcount parked the invoice, kept
its hold, and charged again under a new key once 24 hours of absence had
passed, accepting that a payment Stripe created even later became a duplicate
(R10).

Stripe's one-call recovery is weak among providers. Juspay and PayU answer
"not found" for an id the merchant chose, GoCardless answers a reused key with
the existing payment, and Checkout.com lists payments by the merchant's
reference. Only Square, among eleven other providers checked on 2026-11-10,
offers what Stripe's two calls give: a handle with an id, created before money
moves, that can be canceled into a final "nothing happened" (provider
research report, 2026-11-10).

Verified in Stripe test mode (2026-11-09): a create replays its PaymentIntent
under the same key; confirming a succeeded PaymentIntent returns
`payment_intent_unexpected_state`; a PaymentIntent succeeds once; a canceled
one refuses a confirm; `error_on_requires_action` needs
`payment_method_types=card`; two creates under different keys both charged,
which is why the hold stays.

## Decision

`sendCharge` (internal/payment/stripe.go):

1. `CreateUnconfirmedPaymentIntent` under the attempt's idempotency key. No
   money moves.
2. `StampPendingConfirm` (internal/invoice/postgres.go): one UPDATE that sets
   the invoice to `unknown` on the PaymentIntent and moves the counter, only
   if the hold is still the caller's and not yet ended (`clock_timestamp()`),
   the counter is the one the key was built from, the invoice is finalized
   and pending or failed, and the amount due is the PaymentIntent's. The
   attempt row is written in the same transaction.
3. `ConfirmPaymentIntent` only if the stamp applied.

A stamp that does not apply ends the attempt (`EndUnstampedAttempt`): the
hold is cleared and the counter moved in one compare-and-set, so the next
charge sends a new key. The stamp is refused when the amount due changed
since the charge read the invoice, and Stripe answers the old key with a new
amount as an idempotency error, which reads as an unclear charge. The
PaymentIntent the create made stays unconfirmed at Stripe.

After the confirm: a success settles the invoice; a decline (a card error)
is reported through the settlement primitive; a confirm request Stripe
refuses outright (an invalid request, told by its type and not by whether
the error names the PaymentIntent) marks the invoice failed with no
PaymentIntent, sends no customer email, and cancels the PaymentIntent.
Anything else needs no write, because the invoice already says `unknown` on
the PaymentIntent. The unknown sweep reads it once the hold has ended:
succeeded is recorded, also when the payment was refunded at Stripe since
(the refund is recorded by its own webhook, ADR-134), so the result does not
depend on whether the sweep or the payment's webhook comes first; processing
is followed; one still awaiting its confirm is canceled, read again, and
ended with no customer email (`EndAttemptWithoutPayment`: back to pending, or
failed for a dunning retry); a decline is recorded as the decline it was. A
payment error with no charge (the payment needs the customer to
authenticate) is a decline too, and the customer is told.

The stale-hold sweep makes no Stripe call. Every claim writes
`invoices.charge_hold_token` equal to the hold. A stale hold carrying its
token on an invoice still pending or failed is a charge that never stamped,
so never confirmed: the hold is cleared and the counter moved, by a
compare-and-set on the same row the stamp writes, so exactly one of the two
wins. On any other invoice the hold is a leftover and is cleared. A hold
without its token, or a park, was left by the one-call version: kept and
reported, never resolved on a guess.

The token is transition code. The upgrade check below is the upgrade path;
the token covers the one case that check cannot see: a copy of the previous
version still running against the new schema (a rolling deploy, where the
old copies are being stopped while they may be mid-charge) takes a hold
after the check passed. Without the token the sweep would clear that hold
and the invoice would be charged again while the old copy's one-call charge
may have taken money. The column, its write in the claim, the sweep's
"earlier version" branch and the parked surface can all be removed in the
release after the one that ships this.

Write-off is allowed on any `unknown` invoice while no charge holds it.
Migration 00225 adds the token column and refuses to run while any open or
written-off invoice has a charge hold or a parked charge from the previous
version. Written-off invoices count because the previous version still
settles those (it records a payment it finds, and ends a park) and this
version does not.

## Why this design

Make the record exist before the irreversible step, and put it where every
reader already looks. Once the invoice names the PaymentIntent, "what did
this charge do?" has one answer to read by id, and "nothing happened" is made
final by a cancel instead of assumed after a wait. Time-based guessing (the
24-hour park) becomes state-based fact.

Putting the record on the invoice row, not beside it, is what keeps this
small. The claim, the stamp, every settle and the stale-hold clear are
compare-and-sets on one row, so there is no second record to keep in step
with the hold, and the existing `unknown` state, its sweep and its action
gate (no void, no offline payment, no second charge, credit notes deferred)
apply to a charge in flight without new code. An offline payment or a credit
note that lands before the stamp makes the stamp fail, so nothing is
confirmed; one attempted after it is refused. The one-call design detected
those as second payments after the fact.

The provider-neutral rule is "save the attempt before moving money; resolve
by reading, never by blind retry; cancel where the provider allows". The
create/confirm split is how the Stripe adapter meets it. Other providers meet
it differently: Razorpay orders cannot be canceled, PayPal's documented
recurring charge is one call, and Checkout.com repeats a payment on a same-key
retry after a 5xx. Those adapters will keep a waiting rule where the provider
gives no cancel.

## Alternatives considered

- **A. Keep one call (main before this ADR).** Simplest client, one Stripe
  call per charge. Rejected: it keeps the parked state, the 24-hour wait and
  R10, because nothing named the payment that might still appear.
- **B. One PaymentIntent per invoice (ADR-117).** Every channel confirms the
  same PaymentIntent. Still deferred: the hosted page would move off Checkout,
  amounts would track credit notes, and Stripe cancels a PaymentIntent after
  10 failed confirms, so it needs replacement anyway. This ADR is its
  off-session half.
- **C. Save the PaymentIntent on the attempt row, tagged with the hold.**
  Built first and reviewed three times (2026-11-10). It left the invoice
  pending during the charge, so a second record had to be kept in step with
  the hold: a token, a stamp on the attempt, a guarded save, a re-stamp on a
  replayed create, a locked clear, and a stale-hold sweep that read Stripe
  beside the unknown sweep. Most of the reviews' serious findings were in
  that bookkeeping, and the production code came out 137 lines larger than
  the one-call design. Stamping the invoice instead came out 164 lines
  smaller.
- **D. A one-release transition that resolves the previous version's holds
  and parks through its customer-list search.** Built and tested, then
  dropped: Hardcount has no production deployments, so the upgrade check
  replaces about 330 lines of code that would protect state that does not
  exist and be deleted a release later. What remains of a transition is the
  hold token (Decision): a column, one write, one list predicate and one
  sweep branch that keeps such a hold for a person.

## Consequences

### Positive
- No parked state, no customer search, no 24-hour wait, no R10.
- An unclear charge resolves at the first billing tick after its hold ends:
  minutes, by one read by id.
- One sweep talks to Stripe about a charge, not two.
- The charge no longer saves anything after an unclear answer, so the rule
  about which contexts may save one (ADR-127, "by cause") is gone.
- An offline payment, a void or a credit note cannot land while a confirm is
  at Stripe.

### Risks / open items
- One more Stripe call per charge.
- For the second or so a charge takes, the invoice shows `unknown` ("Stripe
  hasn't confirmed this charge yet"), and the counter moves at the stamp and
  again at a paid or failed outcome.
- A 429, unreadable credentials or a refused key at the confirm leaves the
  invoice `unknown` until its hold ends and the next tick reads the
  PaymentIntent, where the same error at the create is a quiet skip.
- An unconfirmed PaymentIntent a lost create leaves at Stripe stays there,
  uncancelled. It can take nothing (only one stamped on the invoice is ever
  confirmed), but it shows in the Stripe dashboard.
- A payment that succeeds while the invoice was settled by another
  PaymentIntent (a hosted page left payable at Stripe, ADR-068's residual) is
  flagged by its webhook; if that webhook is also lost, nothing reads it.
  The one-call design's customer-list sweep caught that case.
- A confirm and a cancel racing at Stripe: Stripe guarantees one loses; which
  one, and how it reports, needs a live test-mode check.
- Stripe India e-mandates (debits 24+ hours after the call) are untested.
- Four independent reviews (2026-11-10). The fourth, of this form, found no
  path to a confirm without the stamp, and four gaps, fixed with tests: the
  unknown sweep's cool-off was measured on a test clock's simulated instant;
  a confirm request Stripe refused was reported as a bank's decline; the
  upgrade check did not turn the row-level-security bypass on; a late note
  could reopen a closed attempt row.
- The parked-invoice surface (the gate's parked branch, the dashboard banner,
  `hardcount_parked_invoices`) triggers after the upgrade only for a hold or
  a park that a copy of the previous version left by running beside this
  one. It is removed with the hold token in a follow-up change.
- A fifth review, of the pull request (four reviewers, 2026-11-10), found no
  path to a double charge or to a confirm without the stamp, and these,
  fixed with tests: a refused confirm request whose error names the
  PaymentIntent was still reported as a decline; a refused stamp left the
  key to be reused with another amount; the upgrade check let written-off
  holds and parks through; the store still accepted a park; the stamp's
  amount predicate had no test.
- A written-off invoice whose charge is `unknown` is reached more often (a
  write-off is allowed once the hold ends). Its relief drafts wait for the
  unknown sweep (ADR-050 amendment).
- Upgrade: keep the previous version running until it has finished its
  charges, then stop it and start the new one; one version at a time.

## References

- ADR-001, ADR-068, ADR-105, ADR-107, ADR-117, ADR-127.
- Migration 00225 (`charge_hold_token`, the upgrade check).
- [Stripe: advanced error handling](https://docs.stripe.com/error-low-level)
- [Stripe: PaymentIntent lifecycle](https://docs.stripe.com/payments/paymentintents/lifecycle)
- Provider research, 2026-11-10: Razorpay, Cashfree, Juspay, PayU, PayPal,
  Braintree, Adyen, Checkout.com, Mollie, GoCardless, Square.
