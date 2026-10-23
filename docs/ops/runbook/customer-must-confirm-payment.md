# The customer's bank wants them to confirm a payment

Some banks ask the cardholder to confirm a card payment, usually with 3-D
Secure: a code or an app prompt. A charge Velox makes on its own (at
finalize, a payment retry, **Collect payment**) has no customer present to
confirm it, so Stripe refuses it with `authentication_required`. This page
says what Velox does then, and what you can do. Back to the
[runbook index](index.md).

## Symptom

One of these:

- An invoice shows the notice "Customer must confirm payment".
- The invoice's Activity shows "Payment failed · The bank asked the customer
  to confirm this payment", or "Payment retry N not attempted".
- The log line `dunning retry: not charging this card; the bank refused it
  for good or wants the customer to confirm the payment` with
  `decline_code=authentication_required`.

## Why

- **The card works, but only with the customer.** On the hosted invoice
  page, Stripe asks the customer's bank to confirm the payment and shows
  the customer the bank's check. A charge made without them can't pass it.
- **The invoice is a failed payment.** `payment_status` is `failed`, and
  payment recovery starts as for any failed payment.
- **Retries don't charge that card.** Stripe Billing does not retry
  `authentication_required`
  ([Stripe](https://docs.stripe.com/billing/revenue-recovery/smart-retries#non-retryable-decline-codes)),
  and neither does Velox. Each scheduled retry counts as an attempt and
  charges nothing while that card is the customer's default. When the
  attempts run out, the policy's final action runs, as for any failed
  payment.
- **The customer is asked to confirm.** The first email, and each payment
  reminder, says "Your bank asked you to confirm the payment" with a link to
  the hosted invoice page, where they pay and confirm. It says nothing about
  a decline. Emails need SMTP set up and an email address on the customer.
- **A different card ends it.** A card the customer saves through a setup
  or update link becomes the default, and the next retry charges it.
- **The newest answer for the card decides.** If a later charge on the same
  card gets an ordinary decline instead, retries charge the card again.

## Check

Read the invoice:

```bash
curl -s "$API/v1/invoices/$INVOICE_ID" -H "Authorization: Bearer $KEY"
```

**You see:** `invoice.payment_status` `failed`, `invoice.attention.code`
`payment.authentication_required`, and Stripe's message in
`invoice.attention.provider_response`.

To list every such invoice of every tenant (in `psql`), newest answer per
invoice and card:

```sql
SELECT DISTINCT ON (a.invoice_id, a.stripe_payment_method_id)
       i.tenant_id, i.id, i.invoice_number, a.stripe_payment_method_id, a.decline_code, a.created_at
FROM invoice_charge_attempts a
JOIN invoices i ON i.id = a.invoice_id
WHERE i.status = 'finalized' AND i.payment_status = 'failed' AND a.decline_code <> ''
ORDER BY a.invoice_id, a.stripe_payment_method_id, a.created_at DESC;
```

Rows whose `decline_code` is `authentication_required` are the ones.

## Fix

- **Send the customer the payment link.** Use **Email payment link** on the
  notice. The customer pays on the hosted invoice page and confirms with
  their bank there.
- **Or ask for a different card.** **Add payment method** on the customer
  page sends a setup link; the card saved on it becomes the default, and the
  next retry charges it.

Don't use **Collect payment** for this invoice with the same card. It
charges without the customer, Stripe asks for the confirmation again, and
the customer gets another email.

## Verify

Read the invoice again. Once the customer has paid, `status` is `paid` and
`attention` is gone. Once a different card is the default, the notice is the
ordinary "Payment failed" one until the next retry charges that card.
