# Card payment failure handling

This is the operator's reference for each card payment failure case: what
Hardcount does, and the gaps still open. How payments and retries work is
in [Payments](concepts/payments.md).

Hardcount charges cards with Stripe PaymentIntents.

## Rules

1. **An unclear answer is `unknown`, not failed, and it always names the
   payment.** Each charge goes to Stripe in three steps. It creates the
   PaymentIntent unconfirmed, which moves no money. It puts that
   PaymentIntent on the invoice, whose payment becomes `unknown`. Then it
   confirms it, which moves the money. Hardcount confirms only a
   PaymentIntent that is on the invoice
   ([ADR-137](adr/137-charge-stamps-invoice-before-confirming.md)). So the invoice
   says `unknown` ("Confirming payment") for the second or so a charge
   takes, then paid, failed or processing. A Stripe 5xx, any 409, a network
   error or a timeout is an unclear answer.
   - On the create, nothing was confirmed and no money moved. The invoice
     keeps its payment status, and the charge hold stays until the
     reconciler clears it (rule 2).
   - On the confirm, the invoice already says `unknown` on that
     PaymentIntent, so nothing more needs saving. Nothing charges that invoice
     again until a Stripe webhook or the reconciler gives the answer. The
     reconciler is the check that asks Stripe what happened to a payment
     Hardcount has no answer for.
   - A 409 counts as unclear because Stripe sends it when an idempotency key
     is reused, so an earlier request may have charged the card.
   - The confirm can also end with no answer at all: a crash, a shutdown, a
     closed **Collect payment**, a 429, or a key Stripe refuses. The invoice
     is in the same state: `unknown` on that PaymentIntent.
   - What happens next is in the Cases rows "An unclear answer from Stripe"
     and "Crash after Stripe charged, before Hardcount saved".
2. **One charge per invoice at a time, and none while an earlier one is
   unresolved.**
   - Automatic charges, dunning retries and **Collect payment** share one
     claim on the invoice: the charge hold, which lasts 5 minutes. (Dunning
     is Hardcount's retrying of a failed card payment, with emails to the
     customer.)
   - An open hosted Checkout page blocks these charges, and a running or
     `unknown` charge blocks the page
     ([ADR-068](adr/068-checkout-session-dedup.md)).
   - A charge with a definite answer frees its charge hold at once. So does
     a charge that confirmed nothing for one of these reasons: the circuit
     breaker open; the Stripe credentials unreadable; the Stripe key refused
     on the create, or missing; or a 429 on the create. So does one whose
     invoice changed at the re-check before sending, or before the
     PaymentIntent could be put on it, and one whose amount is below
     Stripe's minimum.
   - A charge that stops after its PaymentIntent is on the invoice, with no
     definite answer, keeps its charge hold until the 5 minutes end. The
     invoice is `unknown`, so nothing charges it, and the reconciler reads
     the PaymentIntent once the charge hold has ended (rule 5). A result
     recorded in the meantime (by the payment's webhook or the reconciler)
     frees the charge hold with it. After a failed result, the hosted page,
     **Collect payment**, void and offline payment work again at once.
   - A charge that stops before that, after a crash or an unclear answer to
     the create, leaves its charge hold set on a `pending` or `failed`
     invoice after the 5 minutes.
   - While that charge hold stays set, every charge waits for it, and so do
     the hosted page, void, offline payment and dunning's final action.
   - The reconciler clears it on the first scheduled run at least 10 minutes
     after the charge hold ended. It asks Stripe nothing, because such a
     charge never confirmed a payment. With the hourly run, that is usually
     within about an hour
     ([ADR-127](adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)).
3. **Idempotency key per attempt.** Re-sending the same attempt sends the
   same key, so Stripe returns the first PaymentIntent. The same attempt means
   the same attempt counter, the same card, and the same kind of charge: a
   dunning retry, or any other charge (auto-charge and **Collect payment**
   send the same key). The confirm sends the create's key plus `_confirm`.
   A dunning retry after a decline is a new attempt with a new key. So is
   the next charge after the reconciler cancels a payment that never
   finished, or clears the charge hold of a charge that confirmed nothing.
   Stripe may drop a key once it is 24 hours old.
4. **Webhooks: deduplicated, first result wins.** A processed event ID is
   skipped. A failure arriving after the invoice is paid, or from an older
   attempt, is ignored. A transient processing error gets a 5xx, so Stripe
   redelivers; an event that names no Hardcount invoice is acknowledged.
5. **Charges do not wait on webhooks.** The reconciler asks Stripe on every
   scheduled run: hourly, or every 5 minutes when `APP_ENV` is `local` or
   unset. On each run it reads:
   - `unknown` payments that carry a PaymentIntent ID, once no charge holds
     the invoice (5 minutes after the charge began) and the invoice's last
     change is at least 1 minute old;
   - `processing` payments, once they are at least 30 minutes old;
   - hosted Checkout sessions, once they are at least 5 minutes old.

   On the same run it clears charge holds left on `pending` or `failed`
   invoices, once the charge hold ended 10 minutes ago. That is at least 15
   minutes after the charge began, and it makes no Stripe call (rule 2).

   These are minimum ages: each waits for the next scheduled run. Results go
   through the same settlement code as webhooks. A pending refund's later
   status still comes only from webhooks. A decline the reconciler finds on
   an `unknown` payment is recorded as a failed payment, as its webhook
   would record it.

## Cases

| Case | Hardcount | Operator |
|---|---|---|
| No card on file, money owed | With Stripe connected: the customer is emailed a link to add a card, once, and dunning starts; a card they add is charged on the next run. Without Stripe for the invoice's mode, no card can be added or charged online, even one saved earlier: the customer is emailed the invoice itself (PDF and hosted page link), once, never a setup link. Dunning still starts; its retries charge nothing, each reminder asks them to pay the invoice, and the policy's final action runs when the retries run out ([ADR-060](adr/060-no-payment-method-dunning-enrollment.md)). Neither email is sent without SMTP or a customer email address; the next run tries again. | Without Stripe: **Record offline payment** when the customer pays, or connect Stripe; the next run then emails the setup link. |
| Card declined on a Hardcount charge | Payment `failed`, customer emailed (if SMTP is set), `payment.failed` sent, dunning starts (new accounts' default policy: day 3, 7, 14). A decline on the hosted page leaves the invoice as it was. A card the customer saves through a setup or update link becomes the default; the next retry charges it. | Nothing, or ask for a new card (**Update payment method**). |
| Card refused for good: lost, stolen, pick up card, revoked authorization and the rest of Stripe's hard decline codes | As above, but retries no longer charge that card for this invoice. They keep their schedule and count, and charge only once another card is the customer's default. A card saved after the refusal becomes the default, as above (paying on the hosted page also saves one). The customer is asked for a different card; the reason is not named. The invoice says "needs a different card". | Ask for a new card (**Update payment method**), or let the customer pay on the hosted page. **Collect payment** refuses that card with a 409 (`card_refused`) and charges nothing. A card saved before the refusal needs **Set as default** on the customer's page ([ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md)). |
| The bank wants the customer to confirm the payment (3-D Secure, Stripe's `authentication_required`) | Payment `failed` and dunning starts, as for a decline. Retries count but don't charge that card while it is the default, as in Stripe Billing; a card saved through a setup or update link becomes the default and the next retry charges it. The customer's email and each reminder ask them to confirm on the hosted invoice page, with nothing about a decline. The invoice says "Customer must confirm payment". | **Email payment link**: the customer pays and confirms on the hosted invoice page ([runbook](ops/runbook/customer-must-confirm-payment.md)). |
| Stripe refuses the request itself, not the card (for example, the saved payment method no longer exists at Stripe) | Nothing was charged. Payment `failed` with the reason and no PaymentIntent on the invoice; no payment-failed email, because the customer's bank was never asked. Dunning starts, so the retry schedule tries again. Hardcount asks Stripe to cancel a PaymentIntent that a refused confirm left behind. | Read the payment error on the invoice and fix the cause before the next retry. |
| An unclear answer from Stripe: a 5xx, any 409, a network error, or a request past the caller's time limit (15 s for dunning, 30 s for auto-charge and **Collect payment**) | On the create: nothing was confirmed and no money moved. The reconciler clears the charge hold on its first run at least 10 minutes after the hold ends, without asking Stripe, and the next charge uses a new idempotency key. On the confirm: the invoice already says `unknown` ("Confirming payment") on that PaymentIntent, and nothing charges it. A webhook settles it, or the reconciler reads the PaymentIntent by its ID on the first scheduled run after the charge hold ends, 5 minutes after the charge began. Succeeded marks the invoice paid, a decline is recorded as a failed payment, and processing is read again on each run. A PaymentIntent that never finished is canceled at Stripe, so a late confirm is refused. One already canceled with no charge, or a payment refunded in full at Stripe, ends the same way. The invoice goes back to `pending` (`failed` after a dunning retry), with no payment-failed email. It is charged again as usual under a new idempotency key, possibly in the same run ([ADR-137](adr/137-charge-stamps-invoice-before-confirming.md)). | Nothing. If the customer paid another way, **Mark uncollectible**: it is allowed once the charge hold has ended, 5 minutes after the charge began. If it lasts hours: [runbook](ops/runbook/unclear-charge.md#unconfirmed-with-a-paymentintent). |
| Crash after finalize, before the charge | Finalize and the charge queue are one write; the next run charges it. | Nothing. |
| Crash after Stripe charged, before Hardcount saved; Hardcount stopped waiting for its own reasons (a shutdown, a closed **Collect payment**); or a 429 or a refused key on the confirm | Nothing is lost: the invoice said `unknown` on the charge's PaymentIntent before the confirm was sent. So nothing charges again: not another charge kind, a changed card, the hosted page, or the same attempt after Stripe dropped its key. Void and offline payment are refused; write-off is allowed once the charge hold has ended. A webhook marks it paid. Without one, the reconciler reads the PaymentIntent as in the row above. A crash before the PaymentIntent was on the invoice confirmed nothing: the invoice is still `pending` or `failed`, and the reconciler clears its charge hold without asking Stripe (rule 2). | Nothing. If Stripe is disconnected, its key is rejected, or the PaymentIntent is not in the connected Stripe account, the payment stays `unknown`: [runbook](ops/runbook/unclear-charge.md#unconfirmed-with-a-paymentintent). |
| Concurrent charges (auto, dunning, manual, second replica) | One wins the charge hold; a refused **Collect payment** says a charge is being confirmed with Stripe. | Nothing. |
| Customer pays on the hosted page during a Hardcount charge | Mutually exclusive (see Rules). Sessions expire after 1 hour. | Nothing. |
| Second payment, amount or currency mismatch, money on a voided invoice | "Payment needs review"; `payment.duplicate_charge`, `payment.amount_mismatch`, `payment.currency_mismatch` or `payment.received_on_voided_invoice` sent. | Refund the extra, or reconcile the difference, in Stripe. |
| Customer disputes a card payment (a chargeback: the customer asks their bank to reverse it) | Recorded against the invoice, which stays paid. `payment.dispute_opened` sent, then `payment.dispute_closed` when Stripe closes it. A lost dispute shows a critical notice on the invoice. Needs the three `charge.dispute.*` events on the Stripe webhook endpoint. | Respond in Stripe before the deadline. If lost: a credit note with the money returned outside Hardcount (never a card refund), a new invoice, or a write-off in your own books ([runbook](ops/runbook/card-disputes.md)). |
| Void, write-off or offline payment while a charge is running | Refused until the charge settles: the charge holds the invoice, and the payment is `unknown` from before Stripe is asked to take the money. An invoice whose payment is `unknown` can still be written off once its charge hold has ended, 5 minutes after the charge began. If that charge later turns out to have succeeded, the invoice is marked paid. While a stopped charge's hold stays on a `pending` or `failed` invoice, void and offline payment wait for the reconciler to clear it (rule 2); write-off is allowed once the 5 minutes have ended. | Retry after it settles. |
| Cancel or plan swap that voids an unused prebill during a charge | Refused and rolled back while a charge hold is set on a `pending` or `failed` invoice. That is before a charge has put its payment on the invoice, or after a stopped charge until the reconciler clears the hold (rule 2). A dunning cancel refused this way is tried again 24 hours later. Once the payment is `unknown` or `processing`, the prebill is not voided: the credit for the unused time waits as a draft until the payment's result is known. | Retry the cancel or swap later. |
| Credit note that leaves nothing due on an unpaid invoice (an operator waives it) | The invoice is marked paid with the note. `invoice.paid` is sent, and the invoice in that event has `amount_paid_cents` 0. The invoice's dunning run closes as `payment_recovered`, escalated runs included. The Dunning page's "Recovered" card and the recovery rate leave it out: no money arrived. It can no longer be voided. Revenue does not count the waived amount. A later item removal, quantity cut or downgrade in the same period gives no further credit: the waiver already credited the whole invoice. | Nothing. |
| Credit note while a charge is running, or under a stopped charge's hold | Refused while the payment is `processing` or `unknown`, so also while a charge is at Stripe. Allowed while a charge only holds a `pending` or `failed` invoice: before it has put its payment on the invoice, or after a stopped charge until the reconciler clears the hold. Such a note changes the amount due, so that charge confirms nothing. A note that leaves nothing due then leaves the invoice open at $0; the next charge or dunning retry marks it paid. | Wait for the charge result before issuing one. |
| A dunning retry whose payment Stripe is still processing (a payment method that settles later, such as a bank debit) | The retry counts and dunning keeps running: no `dunning.resolved`, no reminder, and the next retry keeps its date. While the payment is processing, nothing retries the invoice or runs the final action. If it succeeds, dunning closes as recovered then. If it fails, the customer gets that retry's reminder, and the next retry moves to the failure plus the policy's next wait when that is later. After the last retry, the final action runs on a later dunning pass once the retry's charge hold has cleared: usually the first pass after the failure, or the next **Advance** on a test clock. The escalation is dated at the failure. The activity shows "Payment retry N is processing" ([#1289](https://github.com/gethardcount/hardcount/issues/1289)). | Nothing. If it stays processing for days: [runbook](ops/runbook/unclear-charge.md#processing-for-days). |
| Retries used up, and a payment may still go through when dunning's final action is due (`processing`, `unknown`, a charge running, or a stopped charge's hold) | No cancel, pause or write-off while that payment may still go through; dunning checks again later. The final action takes the charge hold itself, so no charge starts while it cancels. A payment that succeeds closes dunning; one that fails gets the final action. An `unknown` payment waits for the reconciler: one that never finished at Stripe goes back to `failed`, and dunning goes on. | Nothing. If you write off an `unknown` invoice whose dunning used every retry, apply the policy's cancel or pause by hand ([runbook](ops/runbook/unclear-charge.md#unconfirmed-with-a-paymentintent)). |
| Stripe failing | Circuit breaker, one for the whole process: after 5 unclear failures in a row within a minute (the count resets every 60 s), no charges for 30 s, and no reconciler reads by PaymentIntent. Those reads count toward the 5 too. The reconciler's clearing of charge holds makes no Stripe call, so the breaker does not stop it. A skipped dunning retry is not counted. | Usually nothing: [runbook](ops/runbook/stripe-circuit-breaker-open.md). |
| Database error during a dunning retry | The retry is skipped and does not use up an attempt; a `WARN` line names the invoice and the error. The next pass usually retries it within the hour. If the write that takes the attempt back fails too, the attempt stays used and an `ERROR` line names the invoice: the run waits for its next scheduled retry, or on the last retry goes to the final action without that charge (see Known limits). If the card was declined and the write after it failed or Hardcount was stopping, the run keeps the policy's wait, and that retry's warning email and timeline row are not written. If the charge hold was saved but the result was lost, the retry waits until the reconciler clears the charge hold: up to about 2 hours. If credits covered the invoice and only marking it paid failed, the next pass marks it paid. | Nothing. |
| A refund made in the Stripe dashboard (or Stripe's API) on a Hardcount payment | Hardcount records the refund as an issued credit note when all of these are true: the refund succeeded; it refunds the payment that paid the invoice; the invoice is paid; it is in the invoice's currency; it fits what is left to refund and credit; and Hardcount hears of it within a day. That credit note's card refund is the Stripe refund, so Hardcount makes no Stripe call. The tax is reversed, and the invoice stays paid. Hardcount records any other refund on the invoice, never as a document. A refund of another payment, or one that failed, shows on the invoice's Activity list. A refund of the invoice's own payment that did not fit shows a notice. `payment.refunded_in_stripe` is sent once. Needs `refund.created` on the Stripe webhook endpoint ([ADR-134](adr/134-refunds-made-in-stripe-are-recorded.md)). | For a notice: record it with a credit note that names the refund, attach it to a note whose refund failed, or dismiss it; never record it by hand. **Retry refund** is refused while one is unsettled ([runbook](ops/runbook/refund-made-in-stripe.md)). |
| Refund pending or failed at the bank | Status follows Stripe's refund webhooks; failed never moves back to succeeded. Failed, or pending over 72 hours, shows on the home page. | **Retry refund** on the credit note. It looks the refund up in Stripe first and reuses it. |
| Refund timeout, Stripe 5xx, 409 or 429 (no clear answer) | Refund `unknown`: the customer may already be refunded. It shows on the home page at once. Webhooks do not change it. | Don't refund in the Stripe dashboard. Retry the refund: Hardcount looks for the refund in Stripe, then resends the same request with the same idempotency key. While Stripe keeps the key (at least 24 hours), it returns the first result instead of making a second refund. |

## Not handled yet

- **The dashboard shows an `unknown` refund as "refund pending"**, with no
  **Retry refund** button, and the "Refunded to Card" card leaves its amount
  out. Retry it with
  `POST /v1/credit-notes/{id}/retry-refund` until the dashboard shows it.
- **Retrying a refund still `unknown` after 24 hours can refund twice.**
  **Retry refund** (only a person starts it; no job does) searches Stripe,
  then resends the same request under the same idempotency key, as it does
  for a refund that never got a result recorded (`none`). Stripe may drop
  the key after 24 hours, so a refund Stripe makes later, while recovering
  from an incident, could be repeated
  ([ADR-107](adr/107-unknown-is-terminal-until-a-human.md#amendment-2026-10-18--the-refund-half-is-done)).
- **An unclear charge stays `unknown` while Stripe cannot show its
  PaymentIntent**: the key revoked, Stripe disconnected, or the account
  switched to another Stripe account. Nothing charges the invoice until then.
  Fix the connection, or write the invoice off
  ([runbook](ops/runbook/unclear-charge.md#unconfirmed-with-a-paymentintent)).
- **A PaymentIntent that a charge created and never put on an invoice can
  stay in the Stripe dashboard as incomplete.** It takes no money: Hardcount
  confirms only a PaymentIntent that is on the invoice.
- **A database restart or failover during a dunning retry can still use up
  the attempt.** If the database is still down when Hardcount undoes the
  attempt, it stays counted. Before the last retry, that retry is skipped
  and the run waits the policy's interval. On the last retry, the policy's
  final action then runs without a final charge. A hard stop of the process
  (a kill, not a shutdown) between counting the attempt and the charge does
  the same, and logs nothing.
- **Collect payment still charges a card whose bank wants the customer to
  confirm.** The invoice notice no longer offers a retry, but the invoice
  page's **Collect payment** reaches Stripe with that card; Stripe asks
  again and the customer gets another email. (A card refused for good is
  refused, [ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md).)
- **A different card the customer pays with on the hosted page does not
  replace a default that needed confirmation.** It pays that invoice; the
  next invoice charges the old default again, and the bank may ask again.
  If they pay with the same card, the copy that payment saved becomes the
  default (Hardcount keeps one copy of a card), and the next retry or invoice
  charges it.
- **The dashboard has no badge for an open dispute.** An open dispute shows
  only as a row in the invoice's Activity list, and the lost-dispute notice
  carries the generic title "Needs attention" ([runbook](ops/runbook/card-disputes.md)).
  Read disputes with `GET /v1/invoices/{id}/disputes`.
- **A refund made in Stripe whose events never reach Hardcount is not
  recorded.** Hardcount does not poll Stripe for refunds. Stripe retries a
  failed delivery for about three days; an endpoint without
  `refund.created` sees most refunds only through a later `refund.updated`,
  more than a day late, so they are flagged rather than recorded
  ([runbook](ops/runbook/refund-made-in-stripe.md)).
- **The Credit notes page cannot filter to notes recorded from Stripe.**
  Each such note carries the tag "Recorded from Stripe". The notes Hardcount
  and the dashboard record have the reason "Refunded in Stripe", which the
  page's search finds; a note linked through the API keeps the reason it was
  given.

**Next:** react to these events in your app with
[webhooks](webhooks.md#event-catalog). Operators:
[runbook](ops/runbook/index.md).
