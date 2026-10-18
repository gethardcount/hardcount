# Interrupted charges

Velox records each card charge on its invoice just before it calls Stripe,
and clears the record once the charge's result is saved. A crash, a deploy, or
a timeout during the call leaves the record. While it is there, no path
charges the invoice again, and the reconciler asks Stripe what happened to the
charge from 10 minutes on. This page explains the log lines and what to do if
a record stays for more than an hour. Back to the [runbook index](index.md).

The decision behind it is
[ADR-127](../../adr/127-record-each-charge-before-calling-stripe.md).

## Symptom

- An operator sees "A charge on this invoice is being confirmed with Stripe.
  Try again later. An interrupted charge is confirmed within about an hour."
  when they void, write off, record an offline payment, issue a credit note,
  collect payment or email the invoice.
- The invoice still shows Pending or Failed, and the hosted page shows no Pay
  button.
- A WARN in the logs that names "the charge record stays" or "the
  interrupted-charge check".

## Why

| Log line | Meaning | Action |
|---|---|---|
| WARN "the charge's outcome is unclear and nothing names its PaymentIntent; the charge record stays" | The call timed out, was cancelled, lost its connection, or Stripe answered 5xx or 409 "key in use" | None: the check answers it |
| WARN "Stripe rate-limited a charge; the charge record stays" | Stripe answered 429. An earlier request under the same key may still be running | None |
| WARN "could not save the charge's outcome; the charge record stays" | Stripe answered, and the database write failed | None |
| ERROR "could not record a declined charge — the charge record stays" | A decline was not saved; the check saves it and starts dunning | None, unless it repeats: check the database |
| WARN "could not clear the charge record" | The result is saved, and only the clearing failed | None: the check clears it |
| ERROR "could not record the charge on the invoice before calling Stripe — not charging" | The record write failed, so Stripe was not called | Check the database. The next run tries again |
| WARN "interrupted charge never reached Stripe; the invoice will be charged normally" | The check found nothing at Stripe and released the invoice | None |
| WARN "interrupted charge found at Stripe waiting, and its invoice moved on; the PaymentIntent is canceled" | A PaymentIntent waiting on the customer was left behind and is now canceled | None |
| WARN "interrupted-charge check could not read Stripe" | Stripe, the customer, or the credentials could not be read | See [Fix](#fix) if it lasts |
| ERROR "more than one PaymentIntent carries one charge's idempotency key" | Should not happen; the check acts on the most advanced one | Look the key up in Stripe |
| CRITICAL "this invoice is parked" | Stripe could not be read about the charge for 24 hours | [Parked invoices](parked-invoices.md) |

`velox_interrupted_charge_resolutions_total{result}` counts what the check
did: `succeeded`, `declined`, `processing`, `canceled`, `not_found` and
`parked`.

## Check

```sql
SELECT id, tenant_id, payment_status, charge_started_at, charge_started_key,
       charge_started_stripe_customer, provider_payment_status, provider_synced_at
FROM invoices
WHERE charge_started_at IS NOT NULL
  AND charge_started_at < now() - interval '1 hour'
ORDER BY charge_started_at
LIMIT 20;
```

A record older than an hour, with `provider_payment_status` set to
`attempt_check_error`, means the check could not read Stripe.

## Fix

1. Confirm the reconciler runs. It runs once per scheduler tick: hourly in
   production, every 5 minutes in local. See
   [A background job is stuck](background-job-stuck.md).
2. Search the logs for "interrupted-charge check could not read Stripe" with
   the invoice id. The error says why:
   - Stripe errors or timeouts: wait for Stripe to recover. The check asks
     again every 10 minutes.
   - The tenant disconnected or changed its Stripe keys for this mode:
     reconnect them under Settings → Payments.
   - "No such customer" or "is deleted": the Stripe customer is gone, or
     belongs to another Stripe account. Find the charge in the Stripe
     dashboard by its idempotency key (`charge_started_key`).
3. Do not charge the invoice by hand in Stripe while the record is there. If
   Stripe cannot be read for 24 hours, the invoice parks, and the
   [Parked invoices](parked-invoices.md) runbook takes over.

## Verify

The query above no longer returns the invoice, and the invoice shows the
result: paid, failed with dunning running, or pending and charged on the next
run.

Related: [Payments stuck as unknown](payment-status-unknown.md),
[Card payment failure handling](../../payments.md).
