# When card payments go wrong

Velox charges your customers' cards through Stripe. This page lists the
payment problems billing systems usually meet, and what Velox does about
each one. They include a declined card, a charge with an unclear result, a
crash in the middle of a charge, Stripe messages that arrive twice or late,
and refunds. For each problem it says what you see and what, if anything,
you do.

**Who it is for:** anyone checking whether Velox is safe to put money
through, and operators running it.

**In short:** when Velox cannot tell whether a charge went through, it does
not charge that invoice again until Stripe's record says what happened. It
is like a cashier whose card reader lost its connection mid-payment: they
check with the bank before trying the card again. If a second payment still
gets through, Velox keeps one payment on the invoice and asks you to refund
the other. Four known gaps are listed under [Not handled yet](#not-handled-yet).

Words used below:

- A **webhook** is a message Stripe sends to Velox when a payment changes.
  Stripe can send one twice, late, or out of order. Velox also sends
  webhooks to your own systems, such as `payment.failed`.
- **Dunning** is Velox retrying a failed payment on a schedule and emailing
  the customer.
- The **hosted payment page** is the page where your customer pays an
  invoice themselves. It is a Stripe Checkout page.
- A **request key** (idempotency key) is a label Velox sends with each
  charge. If Stripe sees the same key again, it returns the first result
  instead of charging again. Stripe keeps a key for about 24 hours.

## Charging a card

| What happens | What Velox does | What you see | What you do |
|---|---|---|---|
| **The card is declined.** | Marks the payment failed and emails the customer (once email is set up). Dunning then retries on your policy's schedule. A new account's default policy retries on day 3, 7 and 14. | "Payment failed" on the invoice, and the `payment.failed` webhook. | Nothing, or ask the customer for a new card. **Collect payment** charges again now. |
| **Stripe times out or answers with a server error**, so the charge may or may not have gone through. | Marks the payment status unknown. Nothing charges the invoice again: not the automatic charge, not dunning, not **Collect payment** and not the hosted page. Stripe's webhook or Velox's own check marks it paid or failed. | "Confirming payment" on the invoice. | Nothing. If it stays for hours, see [Payments stuck as unknown](ops/runbook/payment-status-unknown.md). |
| **The unclear answer has no Stripe payment ID**, so Velox cannot look the charge up. The invoice is now *parked*. | Nothing charges it. From an hour after parking, Velox searches Stripe each hour for charges tagged with the invoice. A successful one marks the invoice paid. Finding nothing never marks it failed, because Stripe's search can lag. | "Payment status unknown" on the invoice, and `velox_parked_invoices` above zero. | Look for the charge in Stripe. If nothing was taken, choose **Mark uncollectible**. See [Parked invoices](ops/runbook/parked-invoices.md). |
| **Velox stops after finalizing an invoice, before charging it**, for example in a crash or restart. | The same database write that finalized the invoice also queued it for charging, so the next scheduled run charges it. | "Payment scheduled" until then. | Nothing. |
| **Velox stops after Stripe took the money, before saving the result.** | Stripe's webhook marks the invoice paid. If no webhook comes, the next charge sends the same request key, so Stripe returns the first payment instead of making a new one. | Nothing unusual. | Nothing. A rare exception is under [Not handled yet](#not-handled-yet). |
| **Two charges start at once**: the automatic charge, a dunning retry, a **Collect payment** click, or a second Velox server. | Each one first takes the same five-minute hold on the invoice, so only one charge runs at a time. | A refused click says a payment is already in progress. | Nothing. |
| **The customer pays on the hosted page while Velox is about to charge the card.** | While a payment page is open, Velox holds its own charges and dunning retries. While a Velox charge is running or unclear, the page takes no payment. Two devices get the same page. | "Customer is paying" on the invoice. | Nothing. A page expires after one hour. |
| **A second payment succeeds on an invoice that is already paid.** | Keeps one payment on the invoice and flags the extra one. It also flags a charge for a different amount than the invoice recorded, and money that arrives on a voided invoice. It never refunds on its own. | "Payment needs review" with **Open in Stripe**, and the `payment.duplicate_charge`, `payment.amount_mismatch` or `payment.received_on_voided_invoice` webhook. | Refund the extra money in Stripe. |
| **You void, write off, issue a credit note or record an offline payment while a card charge is running.** | Refuses until Stripe reports the result, so money cannot land on a voided invoice. You can still write off a parked invoice. | An error asking you to wait for the charge. | Try again once the charge settles. |
| **Stripe is down or keeps failing.** | After 5 unclear failures in a row, Velox stops calling Stripe for 30 seconds (a circuit breaker). A skipped charge changes nothing on the invoice, and a skipped dunning retry does not use up a retry. | `velox_stripe_breaker_state` at 2. | Usually nothing. See [Stripe circuit breaker open](ops/runbook/stripe-circuit-breaker-open.md). |

If you write off a parked invoice and its charge turns out to have
succeeded, Velox still marks the invoice paid.

## Stripe webhooks

| What happens | What Velox does | What you see | What you do |
|---|---|---|---|
| **Stripe sends the same webhook twice.** | Skips an event it has already processed. Two copies that arrive together still record one payment, one receipt and one `payment.succeeded`. | Nothing. | Nothing. |
| **A webhook arrives late, or is lost.** | Most card charges do not wait for it: Stripe's direct answer to the charge marks the invoice paid or failed. For the rest, Velox asks Stripe itself (see [How Velox checks with Stripe](#how-velox-checks-with-stripe)). | "Payment processing" or "Confirming payment" until then. After 6 hours, "Payment processing" becomes a warning. | Nothing. If the warning shows, check the payment in Stripe. |
| **Webhooks arrive out of order.** | A failure that arrives after the invoice is paid is ignored. A failure from an older charge attempt does not overwrite a newer one. | Nothing. | Nothing. |
| **Velox cannot process a webhook**, for example while the database is briefly down. | Answers Stripe with an error and records nothing, so Stripe sends the event again. In live mode Stripe keeps trying for up to three days. | An error in Velox's log. | Nothing, unless the errors continue. |

## Refunds

| What happens | What Velox does | What you see | What you do |
|---|---|---|---|
| **A refund is still pending, or fails later at the bank.** | Records the status Stripe gives, then follows Stripe's refund webhooks to "succeeded" or "failed". A late or repeated webhook never moves a failed refund back to succeeded. | A "refunds need attention" alert on the home page, for a failed refund or one pending over 72 hours. | On the **Credit notes** page, choose **Retry refund** on that credit note. |
| **You retry a refund.** | Looks the refund up at Stripe first. If one exists and has not failed, Velox uses it instead of creating another. | The credit note's refund status. | Nothing more. |

## How Velox checks with Stripe

Velox does not rely on webhooks alone. It asks Stripe about the payments it
is unsure of on every scheduled run: hourly in production, and every 5
minutes when `APP_ENV=local`. It checks:

- **An unclear charge with a Stripe payment ID**, once it is a minute old.
- **A charge still processing** after 30 minutes.
- **A parked invoice**, from an hour after it was parked. Velox searches for
  charges tagged with the invoice and acts only on a charge it finds. Stripe
  does not offer search to every account (for example, businesses in India).
  There, a parked invoice waits for you.
- **A hosted payment page**, 5 minutes after it was opened or last checked.
  A paid page whose webhook was lost is recorded, together with the card
  the customer saved.

Each answer goes through the same step as a Stripe webhook. So a payment
found this way sends the same receipt and webhooks, and starts the same
dunning, as one Stripe reported. A
payment Stripe has not finished, such as one waiting for the customer's bank,
is checked again on the next run.

## Not handled yet

- **A refund whose call times out shows as failed, though Stripe may have
  paid it.** **Retry refund** is safe, because it checks Stripe first. A
  refund made by hand in the Stripe dashboard is not recorded in Velox and
  can pay the customer twice. Check Stripe before you refund there.
  ([#965](https://github.com/getvelox/velox/issues/965))
- **A crash at one moment can, rarely, lead to a second charge.** The
  moment is after Stripe took the money and before Velox saved the result,
  with Stripe's webhook late or lost. Normally the next charge reuses the
  request key. It does not when the customer's default card changed, when a
  different kind of charge runs (for example a dunning retry), or when more
  than a day passed. Stripe then makes a new payment. When the first
  payment's webhook does arrive, Velox flags the second as
  `payment.duplicate_charge`.
  ([#976](https://github.com/getvelox/velox/issues/976))
- **Lost or stolen cards are still retried.** Dunning does not read the
  bank's decline reason, so a card reported lost or stolen is tried until
  the schedule ends. ([#966](https://github.com/getvelox/velox/issues/966))
- **Disputes (chargebacks) are not recorded.** When a customer asks their
  bank to reverse a charge, the invoice stays paid in Velox and no webhook
  is sent. Follow disputes in the Stripe dashboard.
  ([#979](https://github.com/getvelox/velox/issues/979))

Related: the [operations runbook](ops/runbook/index.md), the
[webhook event catalog](webhooks.md#event-catalog), and the decision
records behind this page:
[unclear charges](adr/107-unknown-is-terminal-until-a-human.md),
[searching Stripe](adr/108-parked-invoices-search-and-adopt.md),
[the hosted page](adr/068-checkout-session-dedup.md) and
[refunds](adr/063-refund-status-webhook-reconciliation.md).
