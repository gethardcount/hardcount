# Card payment failure handling

Velox charges cards with Stripe PaymentIntents. How it handles the usual
payment failures, and the gaps still open.

## Rules

- **Each charge is recorded before Velox calls Stripe.** The record is on the
  invoice until the charge's result is saved. While it is there, no path
  charges the invoice again (auto-charge, dunning, **Collect payment**, the
  hosted page), and void, write-off, offline payment, credit notes and invoice
  emails are refused. A crash, a deploy or a timeout during the call leaves
  the record. From 10 minutes on, the reconciler asks Stripe about that charge
  ([ADR-127](adr/127-record-each-charge-before-calling-stripe.md)).
- **An ambiguous result is never a decline.** A Stripe 5xx, any Stripe 409, a
  429, or a request that times out or is cancelled while Velox waits can come
  after a request Stripe is still running, which may charge the card. Velox
  writes nothing for such an answer and keeps the charge's record. When the
  answer names a PaymentIntent, the payment is `unknown` and the reconciler
  reads that PaymentIntent. One answer parks the invoice for an operator: a
  409 "same key, different parameters", which only an attempt in flight
  during an upgrade can get
  ([ADR-107](adr/107-unknown-is-terminal-until-a-human.md)).
- **One charge per invoice at a time.** Auto-charge, dunning retries and
  **Collect payment** share one 5-minute claim on the invoice. An open hosted
  Checkout page blocks them, and a running or `unknown` charge blocks the page
  ([ADR-068](adr/068-checkout-session-dedup.md)). The charge's record keeps
  that true after the claim ends: an interrupted charge blocks every path
  until Stripe has been asked about it.
- **Idempotency key per attempt.** Re-sending the same attempt (same attempt
  counter, charge kind and card) sends the same key, so Stripe returns the
  first PaymentIntent. A dunning retry after a decline is a new attempt with a
  new key. Stripe may drop a key once it is 24 hours old. Each PaymentIntent
  also carries its key as `velox_attempt_key` metadata, which is how the
  reconciler finds an interrupted charge.
- **Webhooks: deduplicated, first result wins.** A processed event ID is
  skipped. A failure arriving after the invoice is paid, or from an older
  attempt, is ignored. A transient processing error gets a 5xx, so Stripe
  redelivers; an event that names no Velox invoice is acknowledged.
- **Charges do not wait on webhooks.** The reconciler asks Stripe on every
  scheduled run: hourly, or every 5 minutes when `APP_ENV` is `local` or
  unset. It first checks charge records older than 10 minutes, by listing the
  customer's PaymentIntents and matching the key. It reads `unknown` payments
  that carry a PaymentIntent ID after 1 minute, `processing` after 30
  minutes, hosted Checkout sessions after 5 minutes, and parked invoices
  through Stripe Search
  ([ADR-108](adr/108-parked-invoices-search-and-adopt.md)). Results go
  through the same settlement code as webhooks. Still webhook-only: refund
  status after a pending refund, and parked invoices on accounts where Stripe
  Search is not offered. A declined charge on a parked invoice is
  webhook-only too: the search adopts only a payment that succeeded or is
  still running.
- **Anomalies are flagged, never refunded automatically**: a second
  successful payment, an amount or currency that differs from the invoice,
  money on a voided invoice.
- **Disputes are recorded, never acted on.** A dispute (chargeback) is a
  customer asking their bank to reverse a payment. Velox records each one
  against the invoice the payment paid, from Stripe's `charge.dispute.*`
  webhooks, in any order. It never changes the invoice or moves money: the
  operator responds in Stripe, and decides what a lost dispute means
  ([runbook](ops/runbook/card-disputes.md)).

## Cases

| Case | Velox | Operator |
|---|---|---|
| Card declined on a Velox charge | Payment `failed`, customer emailed (if SMTP is set), `payment.failed` sent, dunning starts (new accounts' default policy: day 3, 7, 14). A decline on the hosted page leaves the invoice as it was. | Nothing, or ask for a new card. |
| Card refused for good: lost, stolen, pick up card, revoked authorization and the rest of Stripe's hard decline codes | As above, but retries no longer charge that card for this invoice. They keep their schedule and count, and charge only once another card is the customer's default. A card the customer saves after the refusal becomes the default by itself, and the next retry charges it. The customer is asked for a different card; the reason is not named. The invoice says "needs a different card". | Ask for a new card (**Update payment method**), or let the customer pay on the hosted page. A card saved before the refusal needs **Set as default** on the customer's page ([ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md)). |
| Stripe 5xx that names a PaymentIntent | `unknown` ("Confirming payment"); the reconciler reads the PaymentIntent after 1 minute, or a webhook settles it. | Nothing. If it lasts hours: [runbook](ops/runbook/payment-status-unknown.md). |
| A request that times out or is cancelled while Velox waits, or an error with no PaymentIntent ID (a 5xx, a 409 "key in use", a 429) | Nothing is written and the charge's record stays: the invoice shows Pending or Failed, and settling actions are refused with "being confirmed with Stripe". The reconciler checks it from 10 minutes on (below). | Nothing. If it lasts more than an hour: [runbook](ops/runbook/interrupted-charges.md). |
| A 409 "same key, different parameters" with no PaymentIntent ID (an attempt in flight during an upgrade) | Invoice *parked* ("Payment status unknown"); searched in Stripe hourly from 1 hour after parking. A found success marks it paid; a decline is recorded only by its webhook; no result never marks it failed. Stripe Search is not offered to every account (for example, India); there only a webhook can. | Check Stripe; **Mark uncollectible** if nothing was taken: [runbook](ops/runbook/parked-invoices.md). |
| Crash after finalize, before the charge | Finalize and the charge queue are one write; the next run charges it. | Nothing. |
| Crash or timeout after Stripe charged, before Velox saved | Velox records each charge before calling Stripe. Nothing charges the invoice again until that charge is checked. From 10 minutes later the reconciler asks Stripe: a payment Stripe took is recorded, a decline starts dunning, and if Stripe never got the request the invoice is charged normally. If Stripe can't be reached for 24 hours, the invoice is parked. A webhook that arrives first records the result as usual. | Nothing. If Stripe was unreachable for a day: [parked invoices](ops/runbook/parked-invoices.md). |
| Concurrent charges (auto, dunning, manual, second replica) | One wins the claim; a refused **Collect payment** says a payment is in progress. | Nothing. |
| Customer pays on the hosted page during a Velox charge | Mutually exclusive (see Rules). Sessions expire after 1 hour. | Nothing. |
| Second payment, amount or currency mismatch, money on a voided invoice | "Payment needs review"; `payment.duplicate_charge`, `payment.amount_mismatch` or `payment.received_on_voided_invoice` sent. | Refund the extra, or reconcile the difference, in Stripe. |
| Customer disputes a card payment (chargeback) | Recorded against the invoice, which stays paid. `payment.dispute_opened` sent, then `payment.dispute_closed` when Stripe closes it. A lost dispute shows a critical notice on the invoice. Needs the three `charge.dispute.*` events on the Stripe webhook endpoint. | Respond in Stripe before the deadline. If lost: a credit note with the money returned outside Velox (never a card refund), a new invoice, or a write-off in your own books ([runbook](ops/runbook/card-disputes.md)). |
| Void, write-off or offline payment while a charge is running | Refused until the charge settles, or, for an interrupted charge, until the reconciler has checked it (about an hour). A parked invoice can still be written off; if its charge later turns out to have succeeded, it is marked paid. | Retry after it settles. |
| Credit note while a charge is running | Refused while the payment is `processing` or `unknown`, and while a charge's record is on the invoice. An automatic credit note (a downgrade's or a cancel's) is drafted and issued once the charge's result is known; a cancel never voids that invoice meanwhile. | Wait for the charge result before issuing one. |
| Retries used up, and a payment may still go through just before dunning's final action (`processing`, `unknown`, or a charge's record, such as after a crash on the final retry) | No cancel, pause or write-off while that payment may still go through; dunning checks again later. A payment that succeeds closes dunning; one that fails gets the final action. A parked invoice waits for an operator. | Nothing. Writing off a parked invoice whose dunning used every retry: apply the policy's cancel or pause by hand ([runbook](ops/runbook/parked-invoices.md)). |
| Stripe failing | Circuit breaker, one for the whole process: after 5 unclear failures in a row, no charges or reconciler reads for 30 s. A skipped dunning retry is not counted. | Usually nothing: [runbook](ops/runbook/stripe-circuit-breaker-open.md). |
| Refund pending or failed at the bank | Status follows Stripe's refund webhooks; failed never moves back to succeeded. Failed, or pending over 72 hours, shows on the home page. | **Retry refund** on the credit note. It looks the refund up in Stripe first and reuses it. |
| Refund timeout, Stripe 5xx, 409 or 429 (no clear answer) | Refund `unknown`: the customer may already be refunded. It shows on the home page at once. Webhooks do not change it. | Don't refund in the Stripe dashboard. Retry the refund: Velox looks for the refund in Stripe, then resends the same request with the same idempotency key. While Stripe keeps the key (at least 24 hours), it returns the first result instead of making a second refund. |

## Not handled yet

- **The dashboard shows an `unknown` refund as "refund pending"**, with no
  **Retry refund** button, and the "Refunded to Card" card leaves its amount
  out. Retry it with
  `POST /v1/credit-notes/{id}/retry-refund` until the dashboard shows it.
- **A refund still `unknown` after 24 hours is resent**, after a search in
  Stripe finds nothing. So is a refund that never got a result recorded
  (`none`). Stripe may drop the idempotency key by then, so a
  refund Stripe makes later, while recovering from an incident, could be
  repeated
  ([ADR-107](adr/107-unknown-is-terminal-until-a-human.md#amendment-2026-10-18--the-refund-half-is-done)).
- **A charge interrupted while a release rolls out** can be charged again by
  a copy of the previous release, which does not read the charge's record.
  Upgrade with the documented 120-second stop grace period, so charges in
  flight finish first
  ([ADR-127](adr/127-record-each-charge-before-calling-stripe.md#risks-and-residuals)).
- **A dunning retry that comes back `processing` closes dunning as
  recovered**, although the invoice is unpaid. If that payment then fails,
  nothing retries the invoice; collect it by hand
  ([#1289](https://github.com/getvelox/velox/issues/1289)).
- **Collect payment still charges a card refused for good.** The invoice
  notice no longer offers a retry, but the invoice page's **Collect
  payment** reaches Stripe with the refused card
  ([ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md)).
- **The dashboard has no badge for an open dispute.** An open dispute shows
  only as a row in the invoice's Activity list, and the lost-dispute notice
  carries the generic title "Needs attention". The dashboard's list of Stripe
  events to turn on does not name the three dispute events yet, so a setup
  that follows only that list records no disputes; add them by hand
  ([runbook](ops/runbook/card-disputes.md)). Read disputes with
  `GET /v1/invoices/{id}/disputes`.
- **A dispute that Stripe's dispute prevention settles (status
  `prevented`) shows no notice.** Prevention settles a dispute by
  refunding the payment in Stripe, and Velox does not record a refund made
  in Stripe. The dispute is in the invoice's Activity list; record the
  refund with a credit note
  ([ADR-049](adr/049-payment-settlement-primitive.md), "Deferred").

Related: [operations runbook](ops/runbook/index.md),
[webhook events](webhooks.md#event-catalog),
[refunds](adr/063-refund-status-webhook-reconciliation.md).
