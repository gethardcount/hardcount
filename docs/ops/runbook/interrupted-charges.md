# Interrupted charges

A charge is interrupted when Velox sent it to Stripe but did not save the
answer: the process stopped, a time limit ended the call, or Stripe answered
429 to a resent request. Velox then keeps the charge's claim on the invoice
(the charge hold) and asks Stripe what happened before anything charges the
invoice again ([ADR-127](../../adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)).
This page is for when that check cannot finish. Back to the
[runbook index](index.md).

## Symptom

- This ERROR in the logs, every hour for the same invoice:
  `stale charge check: can't confirm an earlier charge with Stripe; write the invoice off or fix the Stripe connection`.
- `velox_stale_charge_hold_resolutions_total{result="cannot_ask"}` rising.
- An invoice stays Pending or Failed. **Collect payment**, void and
  offline payment answer: "A charge on this invoice is being confirmed with
  Stripe." Dunning does not retry it, and its final action waits.

A steady `velox_stale_charge_hold_resolutions_total{result="failed"}` is
Stripe being unreachable; the check retries and needs nothing from you
unless it lasts. When three checks in a row get no answer from Stripe, the
check stops until the next hour and logs `stale charge check: Stripe could
not be reached several times in a row`.

## Why

The reconciler lists the customer's payments in Stripe to learn what the
interrupted charge did. It cannot when:

- the tenant disconnected Stripe for this mode (live or test);
- Stripe rejects the tenant's key (revoked or rotated), or the key is a
  restricted key without permission to read PaymentIntents;
- the Stripe customer is not in this Stripe account (deleted there, or
  created under other credentials).

Until it can ask, Velox will not charge the invoice again: the interrupted
charge may have taken the money.

## Check

```sql
-- Invoices whose charge hold ended more than 10 minutes ago
SELECT i.id, i.tenant_id, i.invoice_number, i.status, i.payment_status,
       i.auto_charge_claimed_until, i.provider_synced_at, c.stripe_customer_id
FROM invoices i
LEFT JOIN customers c ON c.id = i.customer_id
WHERE i.auto_charge_claimed_until < now() - interval '10 minutes'
ORDER BY i.auto_charge_claimed_until;
```

In the Stripe dashboard, open the customer (`stripe_customer_id` above) and
look for a payment whose metadata `velox_invoice_id` is the invoice's id.

## Fix

1. **Stripe disconnected or key rejected:** reconnect Stripe or replace the
   key under Settings → Payments. The next hourly check then finishes on
   its own.
2. **The customer is gone from Stripe, or Stripe will not be reconnected:**
   on an open invoice the check never clears, and offline payment is refused
   while it waits. In this order:
   1. **Mark uncollectible** (write-off). It moves no money and is allowed
      while the check waits.
   2. Wait for the next check. It clears the claim on the written-off
      invoice, with one more ERROR.
   3. If Stripe shows a payment for this invoice, record it with **Record
      offline payment**. That moves the invoice from uncollectible to paid.

Do not clear `auto_charge_claimed_until` by hand: that is what lets the next
charge go out, and the interrupted one may have succeeded.

## Verify

- The query above no longer lists the invoice.
- `velox_stale_charge_hold_resolutions_total` shows `settled`, `released` or
  `cleared` for the run that resolved it.
