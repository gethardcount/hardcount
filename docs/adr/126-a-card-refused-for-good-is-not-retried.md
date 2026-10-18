# ADR-126: A card the bank refused for good is not retried until another card is the default

**Date:** 2026-10-18
**Status:** Accepted

## Summary

When the bank refuses a card for good (lost, stolen, pick up card and the
rest of Stripe's hard decline codes), dunning stops charging that card for
that invoice. Dunning is the automatic retrying of a failed payment. Its
schedule and attempt count carry on as before, but a retry charges nothing
while the refused card is the customer's default. A card the customer then
saves replaces the refused card as the default, and the next scheduled retry
charges it. The customer's emails ask for a different card, and the
invoice's notice tells the operator the same. Any other decline keeps the
normal retry schedule.

## Context

Issue #966. Velox used Stripe's decline code only for the message it showed.
Dunning retried every decline on the card saved at that moment, so a card
reported lost or stolen was charged again on every retry until the schedule
ran out.

What others do (read 2026-10-18):

- **Stripe Billing** does not retry these. "Stripe doesn't retry payments
  if … the issuer returned a hard decline code." The list:
  `incorrect_number`, `lost_card`, `pickup_card`, `stolen_card`,
  `revocation_of_authorization`, `revocation_of_all_authorizations`,
  `authentication_required`, `highest_risk_level`,
  `transaction_not_allowed`. And: "Retries continue to be scheduled, and
  attempt_count continues to increment, but retries only execute after
  detecting a new payment method. Unexecuted retries don't create a new
  Charge."
  ([Automate payment retries](https://docs.stripe.com/billing/revenue-recovery/smart-retries#non-retryable-decline-codes))
- **Visa** puts the same declines, and a few more, in its "issuer will
  never approve" category. Any reattempt there costs the merchant a fee. The
  category: 04 pick up card, 14 invalid account number, 41 lost, 43 stolen,
  46 closed account, 57 transaction not permitted, and the R0, R1 and R3 stop
  and revocation orders. Acquirers publish the list; their copies differ at
  the edges
  ([Evolve](https://developers.getevolved.com/enterprise/docs/visas-processing-integrity-fee-program),
  [TabaPay](https://developers.tabapay.com/docs/excessive-retry-rules),
  [Qualpay](https://help.qualpay.com/help/visas-decline-code-grouping)).
- **Recurly** calls hard declines "permanent authorization failures that
  can't be retried" and points merchants to a dunning email so the customer
  supplies a new card
  ([Recurly](https://www-prod.recurly.com/blog/credit-card-decline-categories/)).

Stripe also asks that `lost_card`, `stolen_card` and `fraudulent` reach the
customer as a plain decline, never with the reason
([Decline codes](https://docs.stripe.com/declines/codes)).

## Decision

1. **The codes.** `domain.IsHardDecline` accepts Stripe Billing's list
   above, except two:
   - `authentication_required` needs the customer to confirm the payment,
     not a new card.
   - `highest_risk_level` is not a decline code. It is Stripe Radar's reason
     for blocking a charge, set on the charge's `outcome.reason` with
     `network_status: not_sent_to_network`
     ([Blocked payments](https://docs.stripe.com/declines#blocked-payments)).
     No bank refused the card, and the code never arrives as a decline code.

   Stripe sends `incorrect_number` in two shapes: as the bank's decline code
   (error code `card_declined`), or as its own error code with no decline
   code ([test cards](https://docs.stripe.com/testing#declined-payments),
   "Incorrect number decline"). The charge call, the webhook and the
   reconciler read both. Any other error code without a decline code is left
   out, so that decline keeps Stripe's message and the normal schedule.
2. **The record.** Migration 181 adds `decline_code` and
   `stripe_payment_method_id` to `invoice_charge_attempts`. Every reporter
   of a decline writes them on the attempt's row, in the transaction that
   records the failure. The charge call takes them from its own error and
   the card it charged. The `payment_intent.payment_failed` webhook and the
   reconciler read `last_payment_error.decline_code` (or its `code`, as
   above) and `last_payment_error.payment_method`. A source that does not know them
   leaves the row's values alone.
3. **The retry.** Before it charges, the dunning retry asks
   `HardDeclineOnCard(invoice, default card)`. When the answer is a hard
   decline code, it releases the charge claim and returns
   `domain.ErrCardNeedsReplacing`: the attempt counts, nothing is charged,
   and the run is scheduled as after any failed retry. When the policy's
   attempts run out, its final actions apply as before.
4. **The customer.** The payment-failed email for a hard decline says the
   bank declined the card and asks for a different one, without the reason.
   The dunning warning after a hard decline, and after each skipped retry,
   asks for a different card and promises no retry date.
5. **The operator.** The invoice notice for a failed payment whose default
   card is refused on that invoice reads "needs a different card", with code
   `payment.card_refused` and only **Update payment method**. It says that
   dunning keeps its schedule only while the invoice's dunning run is active
   and not paused. After the run has run out, while it is paused, or with no
   run, it says no automatic retry is scheduled. The timeline shows a skipped
   retry as not charged.
6. **A new card replaces the refused one.** When a card is saved for a
   customer whose current default card has a hard decline recorded on any of
   their invoices, the saved card becomes the default. The check and the
   switch happen in the attach's own transaction (`paymentmethods` `Upsert`).
   Every way a card is saved goes through it: the setup link, the
   payment-update link, the hosted invoice page and the dashboard's Add card.
   This holds for the store's insert path and for a card already on file
   being saved again, so the customer always keeps exactly one default.
   A working default is never replaced (ADR-021, amended).

## Why this design

- **Per invoice and per card, as Stripe.** The rule answers "would this
  retry charge a card the bank already refused for this invoice?". Another
  card, or the first charge of another invoice, is charged as usual.
- **Counted, never held.** A skipped retry counts toward the policy's
  attempts, like a retry with no card on file (ADR-060). A run that only
  waits for a new card would never end if the card never comes.
- **One fact, read twice.** The retry and the notice read the same attempt
  rows, so the notice says "needs a different card" exactly while the next
  retry would skip the card.
- **The raw code is stored, not a yes or no.** A change to the list applies
  to rows already written.
- **The customer's new card is the one charged.** Stripe Billing runs a
  retry after a hard decline "only after detecting a new payment method",
  and it charges the default
  ([payment method ordering](https://docs.stripe.com/billing/revenue-recovery/smart-retries#payment-method-ordering)).
  A card the bank refused for good can never be charged again, so keeping it
  as the default protects nothing. Leaving the new card beside it meant the
  customer did what the emails asked, retries still skipped, and the
  policy's final action (cancel or write off) ran.
- **The whole customer, for the default only.** Which card is the default is
  a fact about the customer, so the replacement looks at hard declines on
  any of their invoices. That also covers a card paid on the hosted page:
  that invoice is already paid when the card is saved. Charging stays per
  invoice (Alternative A).

## Alternatives considered

- **A. Block the card for the whole customer.** Stop every charge on a card
  once it is refused anywhere. Rejected: Stripe applies the rule per
  invoice, and a customer-wide block needs a rule for when it ends. The cost
  is one more decline on the next invoice's first charge.
- **B. Use Stripe's `advice_code` (`do_not_try_again`).** It carries the card
  network's own retry advice. Rejected for now: the stripe-go version Velox
  pins (v82) does not expose it on its error type, and Stripe's own retry
  rule is the code list.
- **C. Hold the run without counting attempts until a new card arrives.**
  Rejected: nothing ends a run whose card never comes (a liveness sink), and
  it is not what Stripe does.
- **D. Leave a new card beside the refused default (ADR-021's rule) and ask
  the operator to make it the default.** The first version of this ADR did
  that. Rejected in review: the only action the notice offers, **Update
  payment method**, sends the customer a setup link whose card then sat
  beside the refused one, so retries kept skipping and dunning ran out. The
  step that would unblock it was hidden in the notice's details.
- **E. Make every newly saved card the default.** Rejected: that changes
  ADR-021 for every customer, including one whose default card works.
  Replacing only a card the bank refused for good is the smallest change
  that makes the customer's own action work.

## Consequences

### Positive

- A card reported lost or stolen is charged once for an invoice, not on
  every retry.
- The customer is asked for a different card instead of being told "we'll
  try again on" a date when the charge cannot succeed.
- The operator sees why retries are not charging. The notice offers no
  **Retry payment** for the refused card. **Collect payment** on the invoice
  page still charges it (see the risks).
- A card the customer saves after the refusal is charged by the next retry,
  with no operator step.

### Risks / open items

- **A card saved before the refusal stays beside it.** Only a card saved
  after the refusal replaces the refused default. A spare card the customer
  already had needs **Set as default** on the customer page. The notice says
  so.
- **A refusal recorded during an attach can miss it.** The attach reads the
  refusal in its own transaction. A decline that commits after that read
  leaves the new card beside the refused one; the notice then shows, and
  the operator sets the default.
- **The same physical card saved again counts as a new card.** The rule is
  keyed on Stripe's payment method ID. If the customer saves the refused card
  again and Stripe makes a new payment method for it, the next retry charges
  it once, the bank refuses it again, and retries skip it from then on.
- **Replacing the default does not update Stripe's own default** for the
  customer, as a first card's default does not. Velox charges the card it
  names, so this changes only what the Stripe dashboard shows.
- **`authentication_required` keeps the normal schedule.** It needs the
  customer to confirm the payment, not a new card. It needs its own copy.
- **Codes outside Stripe's list keep the normal schedule**, among them
  `stop_payment_order` and `invalid_account`, which Visa's list may cover (R0,
  46), and `restricted_card` and `fraudulent`. Stripe's decline-code page
  treats them as unknown-reason or Radar declines. Add one when a partner's
  declines show it never succeeds on retry.
- **Attempts recorded before migration 181 carry no code**, so a card
  refused before the upgrade is retried once more.
- **An operator's manual charge is not blocked.** **Collect payment** in the
  invoice header still charges the refused card through the same endpoint
  the notice's old **Retry payment** used. Hiding it, or asking to confirm,
  while the notice shows is a dashboard change for a later PR.
- **The migration adds two columns** with a constant default: instant, and
  older copies ignore them.

## References

- Issue #966; ADR-021 (hosted page saves the card; amended 2026-10-18 for this ADR), ADR-060 (dunning with no
  card), ADR-102 (charge attempt rows), ADR-049 (one settlement path for
  every reporter)
- Migration 181 (`00181_charge_attempts_decline_code_and_card.sql`)
- [Stripe: automate payment retries, hard decline codes](https://docs.stripe.com/billing/revenue-recovery/smart-retries#non-retryable-decline-codes)
- [Stripe: decline codes](https://docs.stripe.com/declines/codes)
- [Stripe: card declines, advice codes](https://docs.stripe.com/declines/card#retrying-issuer-declines)
