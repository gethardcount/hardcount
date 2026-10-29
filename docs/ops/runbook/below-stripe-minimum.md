# An invoice is below Stripe's minimum charge

Stripe will not charge an amount below its minimum, for example $0.50 for
an account that settles in USD. This page is for an invoice Stripe refused for that reason: what Hardcount
did, and what you can do with it. Back to the [runbook index](index.md).

## Symptom

One of these:

- An invoice shows the notice "Below Stripe's minimum charge".
- The log line `Stripe refused to charge this invoice because the amount is
  below its minimum charge`.
- A payment recovery run on the Dunning page ended with "Below Stripe's
  minimum charge".
- A customer says the hosted invoice page has no Pay button and reads "This
  amount is below the minimum for a card payment".

## Why

- **Stripe refuses the charge, not the card.** It answers `amount_too_small`
  and creates no payment, so nothing is charged. The minimum depends on the
  currency, and for a charge that Stripe converts, on your Stripe account's
  settlement currency ([Stripe's list](https://docs.stripe.com/currencies#minimum-and-maximum-charge-amounts)).
  A light usage month, a trial tail or a proration can come out below it.
- **Hardcount does not treat it as a failed payment.** The invoice stays
  finalized with `payment_status` `pending`. Hardcount does not charge it again
  automatically, starts no payment recovery, and does not email the
  customer about it. A payment recovery already running on the invoice ends
  with resolution `amount_below_minimum`, at once or at its next scheduled
  step, so its final action (by default, pausing the subscription) does not
  run.
- **Hardcount learns the minimum from Stripe's answer**, so the invoice is first
  charged once. For a customer with no card on file, Hardcount learns it when
  the customer clicks Pay on the hosted page, or when a card is saved and
  charged. Until then the invoice is handled like any card-less invoice
  (payment-method reminders, then the policy's final action).
- **Stripe's answer can change.** For a converted charge the minimum is your
  settlement currency's, so a bank account in the invoice's currency, or a
  move in the exchange rate, can bring the amount over it. Collect payment
  asks Stripe again, and once Stripe gives a different answer the invoice
  no longer reads as below the minimum.

## Check

Read the invoice:

```bash
curl -s "$API/v1/invoices/$INVOICE_ID" -H "Authorization: Bearer $KEY"
```

**You see:** `invoice.status` `finalized`, `invoice.payment_status`
`pending`, `invoice.attention.reason` `amount_below_minimum`, and Stripe's
sentence, which names the minimum, in `invoice.attention.provider_response`
and `invoice.last_payment_error`.

To list every such invoice of every tenant (in `psql`):

```sql
SET app.bypass_rls = 'on';
SELECT tenant_id, id, invoice_number, amount_due_cents, currency, below_minimum_at
FROM invoices
WHERE status = 'finalized' AND payment_status = 'pending' AND below_minimum_at IS NOT NULL
ORDER BY below_minimum_at;
```

## Fix

Choose one for each invoice:

- **Void it.** Use **Void invoice** on the notice, or:

  ```bash
  curl -s -X POST "$API/v1/invoices/$INVOICE_ID/void" -H "Authorization: Bearer $KEY"
  ```

- **Record a payment** the customer made another way: **Record offline
  payment** in the invoice's ⋯ menu, or `POST /v1/invoices/{id}/record-payment`.
- **Bill it with something else.** A cycle invoice is created finalized,
  and only a draft takes new lines, so this amount cannot be added to the
  customer's next cycle invoice. When you next bill this customer by hand,
  put it on that manual invoice instead: create it with
  `POST /v1/invoices` (a draft), add this amount with
  `POST /v1/invoices/{id}/line-items`, finalize it with
  `POST /v1/invoices/{id}/finalize`, then void this one. The total must be
  at or above Stripe's minimum, or Stripe refuses that invoice too.

- **Pay it from credit.** If the customer has a credit balance, or you grant
  one, **Collect payment** applies it and pays the invoice when it covers the
  amount. With too little credit and a card on file, Collect asks Stripe
  again: if Stripe still refuses, it answers `amount_below_minimum` and
  nothing is charged. Any credit it applied stays applied.

The hosted page has no Pay button for this invoice, so don't ask the
customer to pay there. Pay comes back once Collect payment gets a different
answer from Stripe, which needs a card on file.

## Verify

Read the invoice again. After a void, `status` is `voided`; after a recorded
payment, `status` is `paid`. Either way `attention` is gone.
