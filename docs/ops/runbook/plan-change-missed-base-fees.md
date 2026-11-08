# Base fees missed at a scheduled plan change

This page finds the period invoices that an older Hardcount under-billed, and
says how to bill what they missed. Back to the [runbook index](index.md).

A scheduled plan change is a plan change set to take effect at the end of the
current billing period. Before the fix, the period invoice built when one took
effect could miss base fees. It happened when the subscription has more than
one item and bills its base fees in advance. That invoice opens the period
that starts at the change. It billed the base fee of each item whose plan
changed, and the usage of every item, but not the base fee of any other item.

## Symptom

- A customer was billed less than usual for one period: the period that
  started when a plan change on one of their items took effect.
- That invoice has a base fee line only for the items whose plan changed.
  The invoice for the next period bills every item again.

## Why

At the end of a period, the billing run first applies the plan changes due
then, and then builds the period's invoice. The older code built the invoice
from the changed items only, so each other item's base fee was left out.
Base fees billed at the end of the period (in arrears) and usage were billed
in full. Hardcount 0.9.2 and earlier have this bug
([#1620](https://github.com/gethardcount/hardcount/issues/1620)).

## Check

Run in `psql` as the database owner ([Before you start](index.md#before-you-start)).
The query only reads. It lists one row for each base fee an invoice missed.

```sql
SET app.bypass_rls = 'on';

WITH opened AS (
  -- Period invoices whose period starts at a scheduled plan change.
  SELECT i.id, i.tenant_id, i.livemode, i.invoice_number, i.customer_id,
         i.subscription_id, i.billing_period_start AS changed_at, i.currency
  FROM invoices i
  JOIN subscriptions s ON s.id = i.subscription_id
  WHERE i.billing_reason = 'subscription_cycle'
    AND i.status <> 'voided'
    AND (s.canceled_at IS NULL OR s.canceled_at > i.billing_period_start)
    AND EXISTS (SELECT 1 FROM billing_intervals b
                WHERE b.subscription_id = i.subscription_id
                  AND b.source = 'plan_scheduled'
                  AND b.starts_at = i.billing_period_start)
), owed AS (
  -- Each item on the subscription at that instant whose plan bills a base
  -- fee in advance. kept_plan: the item's plan did not change then.
  SELECT o.id AS invoice_id, b.subscription_item_id,
         b.starts_at < o.changed_at AS kept_plan,
         p.name AS plan_name, b.quantity,
         p.base_amount_cents * b.quantity AS base_fee_cents
  FROM opened o
  JOIN billing_intervals b ON b.subscription_id = o.subscription_id
   AND b.starts_at <= o.changed_at
   AND (b.ends_at IS NULL OR b.ends_at > o.changed_at)
  JOIN plans p ON p.id = b.plan_id
  WHERE p.base_bill_timing = 'in_advance' AND p.base_amount_cents > 0
)
SELECT o.tenant_id, o.livemode, o.invoice_number, o.id AS invoice_id,
       o.customer_id, o.subscription_id, o.changed_at,
       w.subscription_item_id AS item_id, w.plan_name, w.quantity,
       w.base_fee_cents, o.currency
FROM opened o
JOIN owed w ON w.invoice_id = o.id AND w.kept_plan
WHERE (SELECT count(*) FROM invoice_line_items li
       WHERE li.invoice_id = o.id AND li.line_type = 'base_fee')
    < (SELECT count(*) FROM owed a WHERE a.invoice_id = o.id)
ORDER BY o.tenant_id, o.changed_at, o.invoice_number, w.subscription_item_id;
```

**You see:** no rows when no invoice was affected. Otherwise each row names
the invoice, the item whose base fee it missed, the item's plan and quantity,
and `base_fee_cents`: the plan's base fee for that quantity, before tax.

- An invoice is listed when it has fewer base fee lines than there were
  items billed in advance at the change. An invoice built after the fix bills
  each of them, so it is not listed.
- `base_fee_cents` is a full period's fee. When the period was shorter than
  a full one, the invoice's other base fee lines say "prorated" with the
  days: the missed fee is prorated the same way.
- `livemode` is `false` for test-mode data.

## Fix

For each missed fee, decide with the account whether to bill it:

1. To bill it, make a one-off invoice for the customer: `POST /v1/invoices`.
2. Add a line with the amount (`POST /v1/invoices/{id}/line-items`). Name
   the plan, the period and the invoice that missed it in its description.
3. Finalize it: `POST /v1/invoices/{id}/finalize`.

To forgive the amount instead, do nothing. The later invoices are correct.

## Verify

The one-off invoice is on the customer's invoice list, finalized, with the
missed amount.

Related: [Invoices](../../concepts/invoices.md).
