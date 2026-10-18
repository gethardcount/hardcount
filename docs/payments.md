# Card payment failure handling

Velox charges cards with Stripe PaymentIntents. How it handles the usual
payment failures, and the gaps still open.

## Rules

- **An ambiguous result is `unknown`, not failed.** A timeout, Stripe 5xx or
  idempotency conflict leaves the payment `unknown`. No path charges that
  invoice again (auto-charge, dunning, **Collect payment**, hosted page) until
  a Stripe webhook or the reconciler resolves it. If neither can, because
  Stripe never received the request, the invoice stays parked until an
  operator marks it uncollectible
  ([ADR-107](adr/107-unknown-is-terminal-until-a-human.md)).
- **One charge per invoice at a time.** Auto-charge, dunning retries and
  **Collect payment** share one 5-minute claim on the invoice. An open hosted
  Checkout page blocks them, and a running or `unknown` charge blocks the page
  ([ADR-068](adr/068-checkout-session-dedup.md)).
- **Idempotency key per attempt.** Re-sending the same attempt (same attempt
  counter, charge kind and card) sends the same key, so Stripe returns the
  first PaymentIntent. A dunning retry after a decline is a new attempt with a
  new key. Stripe may drop a key once it is 24 hours old.
- **Webhooks: deduplicated, first result wins.** A processed event ID is
  skipped. A failure arriving after the invoice is paid, or from an older
  attempt, is ignored. A transient processing error gets a 5xx, so Stripe
  redelivers; an event that names no Velox invoice is acknowledged.
- **Charges do not wait on webhooks.** The reconciler asks Stripe on every
  scheduled run: hourly, or every 5 minutes when `APP_ENV` is `local` or
  unset. It reads `unknown` payments that carry a PaymentIntent ID after 1
  minute, `processing` after 30 minutes, hosted Checkout sessions after 5
  minutes, and parked invoices through Stripe Search
  ([ADR-108](adr/108-parked-invoices-search-and-adopt.md)). Results go
  through the same settlement code as webhooks. Still webhook-only: refund
  status after a pending refund, and parked invoices on accounts where Stripe
  Search is not offered.
- **Anomalies are flagged, never refunded automatically**: a second
  successful payment, an amount or currency that differs from the invoice,
  money on a voided invoice.

## Cases

| Case | Velox | Operator |
|---|---|---|
| Card declined on a Velox charge | Payment `failed`, customer emailed (if SMTP is set), `payment.failed` sent, dunning starts (new accounts' default policy: day 3, 7, 14). A decline on the hosted page leaves the invoice as it was. | Nothing, or ask for a new card. |
| Card refused for good: lost, stolen, pick up card, revoked authorization and the rest of Stripe's hard decline codes | As above, but retries no longer charge that card for this invoice. They keep their schedule and count, and charge only once another card is the customer's default. A card the customer saves after the refusal becomes the default by itself, and the next retry charges it. The customer is asked for a different card; the reason is not named. The invoice says "needs a different card". | Ask for a new card (**Update payment method**), or let the customer pay on the hosted page. A card saved before the refusal needs **Set as default** on the customer's page ([ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md)). |
| Stripe 5xx that names a PaymentIntent | `unknown` ("Confirming payment"); the reconciler reads the PaymentIntent after 1 minute, or a webhook settles it. | Nothing. If it lasts hours: [runbook](ops/runbook/payment-status-unknown.md). |
| Timeout, or an error with no PaymentIntent ID | Invoice *parked* ("Payment status unknown"); searched in Stripe hourly from 1 hour after parking. A found success marks it paid; no result never marks it failed. If Stripe never received the request, nothing resolves it. Stripe Search is not offered to every account (for example, India); there only a webhook can. | Check Stripe; **Mark uncollectible** if nothing was taken: [runbook](ops/runbook/parked-invoices.md). |
| Crash after finalize, before the charge | Finalize and the charge queue are one write; the next run charges it. | Nothing. |
| Crash after Stripe charged, before Velox saved | Webhook marks it paid; without one, re-sending the attempt reuses the key. See #976 below. | Nothing. |
| Concurrent charges (auto, dunning, manual, second replica) | One wins the claim; a refused **Collect payment** says a payment is in progress. | Nothing. |
| Customer pays on the hosted page during a Velox charge | Mutually exclusive (see Rules). Sessions expire after 1 hour. | Nothing. |
| Second payment, amount or currency mismatch, money on a voided invoice | "Payment needs review"; `payment.duplicate_charge`, `payment.amount_mismatch` or `payment.received_on_voided_invoice` sent. | Refund the extra, or reconcile the difference, in Stripe. |
| Void, write-off or offline payment while a charge is running | Refused until the charge settles. A parked invoice can still be written off; if its charge later turns out to have succeeded, it is marked paid. | Retry after it settles. |
| Credit note while a charge is running | Refused while the payment is `processing` or `unknown`; allowed while the charge call itself is in flight. | Wait for the charge result before issuing one. |
| Stripe failing | Circuit breaker, one for the whole process: after 5 unclear failures in a row, no charges or reconciler reads for 30 s. A skipped dunning retry is not counted. | Usually nothing: [runbook](ops/runbook/stripe-circuit-breaker-open.md). |
| Refund pending or failed at the bank | Status follows Stripe's refund webhooks; failed never moves back to succeeded. Failed, or pending over 72 hours, shows on the home page. | **Retry refund** on the credit note. It looks the refund up in Stripe first and reuses it. |

## Not handled yet

- **Refund timeout shows `failed`** though Stripe may have paid it. **Retry
  refund** is safe; a manual refund in the Stripe dashboard can pay twice
  ([#965](https://github.com/getvelox/velox/issues/965)).
- **Rare double charge after a crash** between Stripe charging and Velox
  saving, if the webhook is also late or lost. The retry then gets a new
  idempotency key when the default card changed, the charge kind differs (for
  example a dunning retry) or Stripe has dropped the key. The second payment
  is flagged as `payment.duplicate_charge` when the first one's webhook
  arrives; if it never arrives, nothing flags it
  ([#976](https://github.com/getvelox/velox/issues/976)).
- **Collect payment still charges a card refused for good.** The invoice
  notice no longer offers a retry, but the invoice page's **Collect
  payment** reaches Stripe with the refused card
  ([ADR-126](adr/126-a-card-refused-for-good-is-not-retried.md)).
- **Disputes are not recorded**: the invoice stays paid and no webhook is
  sent; follow disputes in Stripe
  ([#979](https://github.com/getvelox/velox/issues/979)).

Related: [operations runbook](ops/runbook/index.md),
[webhook events](webhooks.md#event-catalog),
[refunds](adr/063-refund-status-webhook-reconciliation.md).
