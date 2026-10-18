# Card payment failure handling

Velox charges cards with Stripe PaymentIntents. How it handles the usual
payment failures, and the gaps still open.

## Rules

- **An ambiguous result is `unknown`, not failed.** A timeout, Stripe 5xx or
  idempotency conflict leaves the payment `unknown`. No path charges that
  invoice again (auto-charge, dunning, **Collect payment**, hosted page) until
  a Stripe webhook or the reconciler resolves it
  ([ADR-107](adr/107-unknown-is-terminal-until-a-human.md)).
- **One charge per invoice at a time.** Auto-charge, dunning retries and
  **Collect payment** share one 5-minute claim on the invoice. An open hosted
  Checkout page blocks them, and a running or `unknown` charge blocks the page
  ([ADR-068](adr/068-checkout-session-dedup.md)).
- **Stable idempotency key per attempt.** A retried charge sends the same key,
  so Stripe returns the first PaymentIntent. Stripe keeps keys about 24 hours.
- **Webhooks: deduplicated, first result wins.** A processed event ID is
  skipped. A failure arriving after the invoice is paid, or from an older
  attempt, is ignored. A webhook Velox cannot process gets a 5xx and nothing
  is recorded, so Stripe redelivers.
- **Webhooks are not required.** The reconciler asks Stripe on every
  scheduled run (hourly; 5 minutes with `APP_ENV=local`): `unknown` payments
  after 1 minute, `processing` after 30 minutes, hosted Checkout sessions
  after 5 minutes, and parked invoices through Stripe Search
  ([ADR-108](adr/108-parked-invoices-search-and-adopt.md)). Results go
  through the same settlement code as webhooks.
- **Anomalies are flagged, never refunded automatically**: a second
  successful payment, an amount that differs from the invoice, money on a
  voided invoice.

## Cases

| Case | Velox | Operator |
|---|---|---|
| Card declined | Payment `failed`, customer emailed (if SMTP is set), `payment.failed` sent, dunning starts (default: day 3, 7, 14). | Nothing, or ask for a new card. |
| Timeout or Stripe 5xx | `unknown` ("Confirming payment"); resolved by webhook or reconciler. | Nothing. If it lasts hours: [runbook](ops/runbook/payment-status-unknown.md). |
| Unclear result with no PaymentIntent ID | Invoice *parked*; searched in Stripe hourly from 1 hour after parking. A found success marks it paid; no result never marks it failed. Stripe Search is not offered to every account (for example, India); there it waits for the operator. | Check Stripe; **Mark uncollectible** if nothing was taken: [runbook](ops/runbook/parked-invoices.md). |
| Crash after finalize, before the charge | Finalize and the charge queue are one write; the next run charges it. | Nothing. |
| Crash after Stripe charged, before Velox saved | Webhook marks it paid; without one, the retry reuses the key. See #976 below. | Nothing. |
| Concurrent charges (auto, dunning, manual, second replica) | One wins the claim; a refused **Collect payment** says a payment is in progress. | Nothing. |
| Customer pays on the hosted page during a Velox charge | Mutually exclusive (see Rules). Sessions expire after 1 hour. | Nothing. |
| Second payment, amount mismatch, money on a voided invoice | "Payment needs review"; `payment.duplicate_charge`, `payment.amount_mismatch` or `payment.received_on_voided_invoice` sent. | Refund the extra in Stripe. |
| Void, write-off, credit note or offline payment during a charge | Refused until the charge settles. A parked invoice can still be written off; if its charge later turns out to have succeeded, it is marked paid. | Retry after it settles. |
| Stripe failing | Circuit breaker: after 5 unclear failures in a row, no Stripe calls for 30 s. A skipped dunning retry is not counted. | Usually nothing: [runbook](ops/runbook/stripe-circuit-breaker-open.md). |
| Refund pending or failed at the bank | Status follows Stripe's refund webhooks; failed never moves back to succeeded. Failed, or pending over 72 hours, shows on the home page. | **Retry refund** on the credit note. It looks the refund up in Stripe first and reuses it. |

## Not handled yet

- **Refund timeout shows `failed`** though Stripe may have paid it. **Retry
  refund** is safe; a manual refund in the Stripe dashboard can pay twice
  ([#965](https://github.com/getvelox/velox/issues/965)).
- **Rare double charge after a crash** between Stripe charging and Velox
  saving, if the webhook is also late or lost. The retry then gets a new
  idempotency key when the default card changed, the charge kind differs (for
  example a dunning retry) or more than a day passed. The second payment is
  flagged as `payment.duplicate_charge` when the first one's webhook arrives
  ([#976](https://github.com/getvelox/velox/issues/976)).
- **Hard declines are retried**: dunning ignores decline codes such as
  `lost_card` and `stolen_card` ([#966](https://github.com/getvelox/velox/issues/966)).
- **Disputes are not recorded**: the invoice stays paid and no webhook is
  sent; follow disputes in Stripe
  ([#979](https://github.com/getvelox/velox/issues/979)).

Related: [operations runbook](ops/runbook/index.md),
[webhook events](webhooks.md#event-catalog),
[refunds](adr/063-refund-status-webhook-reconciliation.md).
