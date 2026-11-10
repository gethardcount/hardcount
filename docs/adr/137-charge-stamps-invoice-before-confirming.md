# ADR-137: A charge puts its PaymentIntent on the invoice before confirming it

**Date:** 2026-11-10
**Status:** Proposed
**Supersedes:** [ADR-107](107-unknown-is-terminal-until-a-human.md)'s parked
state and [ADR-127](127-a-lost-charge-result-is-resolved-by-asking-stripe.md)'s customer-list
check, for charges this version makes.
**Amends:** [ADR-001](001-paymentintent-only-stripe.md) (a charge is two
PaymentIntent calls), [ADR-105](105-charge-idempotency-key-seed.md) (the
counter moves only on PaymentIntent stamps and the stale-hold clear),
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

After the confirm: a success settles the invoice; a decline is reported
through the settlement primitive; anything else needs no write, because the
invoice already says `unknown` on the PaymentIntent. The unknown sweep reads
it once the hold has ended: succeeded is recorded (unless refunded in full at
Stripe); processing is followed; one still awaiting its confirm is canceled,
read again, and ended with no customer email (`EndAttemptWithoutPayment`:
back to pending, or failed for a dunning retry); a decline is recorded as the
decline it was.

The stale-hold sweep makes no Stripe call. Every claim writes
`invoices.charge_hold_token` equal to the hold. A stale hold carrying its
token on an invoice still pending or failed is a charge that never stamped,
so never confirmed: the hold is cleared and the counter moved, by a
compare-and-set on the same row the stamp writes, so exactly one of the two
wins. On any other invoice the hold is a leftover and is cleared. A hold
without its token, or a park, was left by the one-call version: kept and
reported, never resolved on a guess.

Write-off is allowed on any `unknown` invoice while no charge holds it.
Migration 00224 adds the token column and refuses to run while any invoice
still open has a charge hold or a parked charge from the previous version.

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
  exist and be deleted a release later.

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
  `hardcount_parked_invoices`) can no longer trigger after the upgrade and is
  removed in a follow-up change.
- Upgrade: keep the previous version running until it has finished its
  charges, then stop it and start the new one; one version at a time.

## Amendment 2026-11-10: a settle frees the hold of the attempt it ends

The hold outlived the answer that ended its attempt. A confirm with no answer
keeps its hold; when the result then arrived by webhook, the invoice went to
`failed` (or `paid`) and the hold stayed until it ended and the stale-hold
sweep cleared it, which the sweep counted as "never confirmed a payment". For
up to an hour the hosted page, Collect, void and offline payment were refused
on an invoice whose customer had just been told the payment failed.

The settle now clears `auto_charge_claimed_until` in the write that records
the result: always when it marks the invoice paid, and when it marks it failed
only if the invoice was in flight (`unknown` or `processing`) on that
PaymentIntent. No claim admits an in-flight invoice, so a hold on one is that
attempt's. A failure report in any other state leaves the hold, which may be a
charger's that has claimed and not yet stamped. The owner's own release is a
compare-and-set on its hold value and finds nothing to do. The release that
existed only for a dunning retry whose payment came back processing
(`ReleaseRetryChargeHoldTx`) is removed.

## References

- ADR-001, ADR-068, ADR-105, ADR-107, ADR-117, ADR-127.
- Migration 00224 (`charge_hold_token`, the upgrade check).
- [Stripe: advanced error handling](https://docs.stripe.com/error-low-level)
- [Stripe: PaymentIntent lifecycle](https://docs.stripe.com/payments/paymentintents/lifecycle)
- Provider research, 2026-11-10: Razorpay, Cashfree, Juspay, PayU, PayPal,
  Braintree, Adyen, Checkout.com, Mollie, GoCardless, Square.
