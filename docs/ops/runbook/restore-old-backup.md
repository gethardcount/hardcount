# Restore a backup older than 26 days

This page finishes a restore whose backup is too old for `hardcount restore catch-up`: you match Hardcount with Stripe by hand. It replaces step 5 of [Restore Hardcount](../restore.md#the-steps-in-order), the catch-up. Its step 1 starts Hardcount (step 6 of the restore), and its last step goes back to the restore at step 7. Back to the [runbook index](index.md).

## Symptom

`hardcount restore catch-up` exits `2` with `the catch-up would read Stripe events from …, more than 29 days ago, and Stripe lists events for 30 days only`.

## Why

The catch-up reads Stripe's events from 3 days before the backup. Stripe lists events for 30 days only, and the catch-up keeps a day of margin, so it refuses a backup more than 26 days old. Until you finish, keep Hardcount from charging on the restored data:

- billing and dunning stay paused, as the restore left them;
- [Start Hardcount](../restore.md#start-hardcount) blocks the payment pages at your proxy and holds your app's calls that charge or refund, until step 6 here;
- only the person doing this restore uses the dashboard, and only for these steps. Its buttons, such as **Charge now**, charge cards.

A payment page already open at Stripe stays open: blocking `/v1/public/` does not close a `checkout.stripe.com` page. Hardcount opens each one for an hour, so step 2 waits until the last one has closed.

## Fix

1. **Remove the marker,** so Hardcount can start: `DELETE FROM restore_catchup;`. Then [start Hardcount](../restore.md#start-hardcount), with its holds.
2. **List the payments Stripe took since 3 days before the backup,** in each Stripe account your Hardcount accounts use (`SET app.bypass_rls = 'on'; SELECT tenant_id, stripe_account_id FROM stripe_provider_credentials WHERE livemode;`). Do it at least an hour after the old server stopped, so that its last payment page has closed. If you do not know when it stopped, wait an hour after step 1. In the Stripe Dashboard, open **Payments** and filter by **Succeeded** and date. Skip a payment whose metadata has no `hardcount_invoice_id`: Hardcount did not make it.
3. **Look up each payment's invoice:** `SET app.bypass_rls = 'on'; SELECT id, status, stripe_payment_intent_id FROM invoices WHERE id = 'hc_inv_…';`

   | You see | Do |
   |---|---|
   | `paid`, and `stripe_payment_intent_id` is this payment's ID | Nothing |
   | `paid` another way: another payment's ID, or none | The customer paid twice. If neither payment has a refund or a dispute, refund this one in full. |
   | `finalized` or `uncollectible` | Record the payment on the invoice ([Mark an invoice paid](manual-interventions.md#mark-an-invoice-paid-offline-payment-received)), with the payment's ID in the note. |
   | `draft`, any other status, or no row | Handle it as a [listed payment](../restore.md#payments-whose-invoice-the-restore-lost). |

4. **Record the refunds made since the backup.** List them in each Stripe account, with the Stripe CLI and that account's live secret key. `1792119600` is 03:00 UTC on 16 October 2026, 3 days before a backup made at 03:00 UTC on 19 October:

   ```bash
   stripe refunds list --api-key sk_live_… --limit 100 -d "created[gte]=1792119600"
   ```

   If the answer says `"has_more": true`, run it again with `-d "starting_after=re_…"`, the last refund's ID. For each refund, look up the invoice of its `payment_intent`, and whether the restored database holds the refund:

   ```sql
   SET app.bypass_rls = 'on';
   SELECT id, tax_transaction_id FROM invoices WHERE stripe_payment_intent_id = 'pi_…';
   SELECT invoice_id FROM credit_notes WHERE stripe_refund_id = 're_…'
   UNION ALL SELECT invoice_id FROM invoice_stripe_refunds WHERE stripe_refund_id = 're_…';
   ```

   | You see | Do |
   |---|---|
   | No invoice | See below the table. |
   | A row in the second query | Nothing: the restored database holds the refund. |
   | No row, and the refund's metadata has `hardcount_cn_id` | Handle it as `made by Hardcount for credit note` in [Refunds a restore's catch-up did not apply](restore-listed-refunds.md#fix), with the invoice the first query found. |
   | No row, and the invoice's `tax_transaction_id` is set | Handle it as `made in Stripe; the invoice was reported to Stripe Tax` on that page: look for the old server's tax reversal first. |
   | No row otherwise | Record it with step 1 of [A refund made in Stripe](refund-made-in-stripe.md#fix). |

   A refund of a payment no invoice holds belongs to step 3. It lowers what is left of a payment you handle as a listed payment. If you recorded its payment with **Mark an invoice paid**, the invoice still shows the whole payment. Record the refund in your own books, and lower any later refund or credit on that invoice by its amount, by hand.
5. **Handle the disputes and saved cards since the backup.** Hardcount knows of neither. List them from the same start time, as in step 4:

   ```bash
   stripe disputes list --api-key sk_live_… --limit 100 -d "created[gte]=1792119600"
   stripe setup_intents list --api-key sk_live_… --limit 100 -d "created[gte]=1792119600"
   ```

   - **A dispute of a Hardcount payment:** follow the Fix of [Card disputes](card-disputes.md#fix), and respond in Stripe before its deadline. A dispute of a payment you handle as a listed payment also lowers what is left of it.
   - **A SetupIntent with `status` `succeeded` and a `hardcount_customer_id` in its metadata:** a card saved since the backup. Send that customer **Send setup link**. A payment from step 2 made on a payment page can save the card too: send its customer the link as well.
6. **Go on with step 7 of the restore,** [Redo the changes made after the backup](../restore.md#redo-the-changes-made-after-the-backup). Do its step 1 first. Its step 2 is what steps 3 to 5 here did. Then its step 3 removes the `respond` line and ends the holds.

## Verify

The [Resume billing, then dunning](../restore.md#resume-billing-then-dunning) checks of the restore pass: `due` falls to 0 once billing has run.
