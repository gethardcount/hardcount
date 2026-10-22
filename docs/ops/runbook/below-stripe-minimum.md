# An invoice is below Stripe's minimum charge

Stripe will not charge an amount below its minimum, for example $0.50 for
an account that settles in USD. This page is for an invoice Stripe refused for that reason: what Velox
did, and what you can do with it. Back to the [runbook index](index.md).

## Symptom

One of these:

- An invoice shows the notice "Below Stripe's minimum charge".
- The log line `Stripe refused to charge this invoice because the amount is
  below its minimum charge`.
- A payment recovery run on the Dunning page ended with "Below Stripe's
  minimum".
- A customer says the hosted invoice page has no Pay button and reads "This
  amount is below the minimum for a card payment".

## Why

- **Stripe refuses the charge, not the card.** It answers `amount_too_small`
  and creates no payment, so nothing is charged. The minimum depends on the
  currency, and for a charge that Stripe converts, on your Stripe account's
  settlement currency ([Stripe's list](https://docs.stripe.com/currencies#minimum-and-maximum-charge-amounts)).
  A light usage month, a trial tail or a proration can come out below it.
- **Velox does not treat it as a failed payment.** The invoice stays
  finalized with `payment_status` `pending`. Velox does not charge it again
  automatically, starts no payment recovery, and does not email the
  customer about it. A payment recovery already running on the invoice ends
  with resolution `amount_below_minimum`, so its final action (by default,
  pausing the subscription) does not run.
- **Velox learns the minimum from Stripe's answer**, so the invoice is first
  charged once. For a customer with no card on file, Velox learns it when
  the customer clicks Pay on the hosted page, or when a card is saved and
  charged. Until then the invoice is handled like any card-less invoice
  (payment-method reminders, then the policy's final action).

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
- **Collect it on the next invoice.** Add the amount as a line on the
  customer's next invoice by hand, then void this one.

- **Pay it from credit.** If the customer has a credit balance, or you grant
  one, **Collect payment** applies it and pays the invoice when it covers the
  amount. With too little credit, Collect answers `amount_below_minimum`
  without asking Stripe again; any credit it applied stays applied.

The hosted page has no Pay button for this invoice, so don't ask the
customer to pay there.

## Verify

Read the invoice again. After a void, `status` is `voided`; after a recorded
payment, `status` is `paid`. Either way `attention` is gone.
