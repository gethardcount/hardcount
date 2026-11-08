# Restore a backup older than 29 days

This page finishes a restore whose backup is too old for `hardcount restore catch-up`: you match Hardcount with Stripe by hand. It replaces step 4 of [Restore Hardcount](../restore.md#the-steps-in-order) and goes back to it at step 7. Back to the [runbook index](index.md).

## Symptom

`hardcount restore catch-up` exits `2` with `the catch-up would read Stripe events from …, more than 29 days ago, and Stripe lists events for 30 days only`.

## Why

Stripe lists events for 30 days only, so the catch-up cannot read what Stripe did since a backup this old. Until you finish, keep Hardcount from charging on the restored data:

- billing and dunning stay paused, as the restore left them;
- block the payment pages, the paths that start with `/v1/public/`, at your proxy. With the one-VM Caddy, add `respond /v1/public/* 503` inside the site block of `/etc/caddy/Caddyfile`, above `reverse_proxy`, and run `sudo systemctl reload caddy`;
- keep your team out of the dashboard, whose buttons, such as **Charge now**, charge cards.

## Fix

1. **Remove the marker,** so Hardcount can start: `DELETE FROM restore_catchup;`. Then [check the database](../restore.md#check-the-restored-database) and [start Hardcount](../restore.md#start-hardcount).
2. **List the payments Stripe took since a day before the backup,** in each Stripe account your Hardcount accounts use (`SET app.bypass_rls = 'on'; SELECT tenant_id, stripe_account_id FROM stripe_provider_credentials WHERE livemode;`). In the Stripe Dashboard, open **Payments** and filter by **Succeeded** and date. Skip a payment whose metadata has no `hardcount_invoice_id`: Hardcount did not make it.
3. **Look up each payment's invoice:** `SET app.bypass_rls = 'on'; SELECT id, status, stripe_payment_intent_id FROM invoices WHERE id = 'hc_inv_…';`

   | You see | Do |
   |---|---|
   | `paid`, and `stripe_payment_intent_id` is this payment's ID | Nothing |
   | `paid` another way: another payment's ID, or none | The customer paid twice. If neither payment has a refund or a dispute, refund this one in full. |
   | `finalized` or `uncollectible` | Record the payment on the invoice ([Mark an invoice paid](manual-interventions.md#mark-an-invoice-paid-offline-payment-received)), with the payment's ID in the note. |
   | `draft`, any other status, or no row | Handle it as a [listed payment](../restore.md#payments-whose-invoice-the-restore-lost). |

4. **Record the refunds made since the backup** on their invoices, as in [A refund made in Stripe](refund-made-in-stripe.md). A refund whose metadata has `hardcount_cn_id` came from a credit note: if that note is still `issued`, click **Retry refund** on it, and do not refund again.
5. **Remove the `respond` line,** then go on with [Redo the changes made after the backup](../restore.md#redo-the-changes-made-after-the-backup), step 7 of the restore.

## Verify

The [Resume billing, then dunning](../restore.md#resume-billing-then-dunning) checks of the restore pass: `due` falls to 0 once billing has run.
